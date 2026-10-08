// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"
	"fmt"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

func isGRPCRequest(r *http.Request) bool {
	if r.ProtoMajor != 2 || r.Method != http.MethodPost {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	codec, extended := strings.CutPrefix(mediaType, "application/grpc+")
	return mediaType == "application/grpc" || (extended && codec != "")
}

func writeProxyError(w http.ResponseWriter, r *http.Request, err *Error) {
	if !isGRPCRequest(r) {
		WriteJSONError(w, err)
		return
	}
	if ctxErr := grpcContextError(r.Context()); ctxErr != nil {
		code := codes.Canceled
		if ctxErr == context.DeadlineExceeded {
			code = codes.DeadlineExceeded
		}
		writeGRPCError(w, code, ctxErr.Error())
		return
	}
	code := codes.Internal
	switch err.Status {
	case http.StatusBadRequest:
		code = codes.InvalidArgument
	case http.StatusUnauthorized:
		code = codes.Unauthenticated
	case http.StatusForbidden:
		code = codes.PermissionDenied
	case http.StatusBadGateway:
		code = codes.Unavailable
	}
	writeGRPCError(w, code, err.Detail)
}

func writeGRPCError(w http.ResponseWriter, code codes.Code, message string) {
	// Use terminal trailers even if middleware flushes the initial headers.
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Grpc-Status", strconv.Itoa(int(code)))
	w.Header().Set("Grpc-Message", url.PathEscape(message))
}

func grpcContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func grpcRequestContext(r *http.Request, budgetCap time.Duration) (context.Context, context.CancelFunc, error) {
	var deadline time.Time
	now := time.Now()
	if budgetCap > 0 {
		deadline = now.Add(budgetCap)
	}
	values := r.Header.Values("Grpc-Timeout")
	if len(values) > 1 {
		return nil, nil, fmt.Errorf("invalid grpc-timeout: multiple values")
	}
	if len(values) == 1 {
		duration, err := parseGRPCTimeout(values[0])
		if err != nil {
			return nil, nil, err
		}
		callerDeadline := now.Add(duration)
		if deadline.IsZero() || callerDeadline.Before(deadline) {
			deadline = callerDeadline
		}
	}
	if deadline.IsZero() {
		ctx, cancel := context.WithCancel(r.Context())
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	return ctx, cancel, nil
}

func parseGRPCTimeout(raw string) (time.Duration, error) {
	if len(raw) < 2 || len(raw) > 9 {
		return 0, fmt.Errorf("invalid grpc-timeout")
	}
	var unit time.Duration
	switch raw[len(raw)-1] {
	case 'H':
		unit = time.Hour
	case 'M':
		unit = time.Minute
	case 'S':
		unit = time.Second
	case 'm':
		unit = time.Millisecond
	case 'u':
		unit = time.Microsecond
	case 'n':
		unit = time.Nanosecond
	default:
		return 0, fmt.Errorf("invalid grpc-timeout unit")
	}
	for _, digit := range raw[:len(raw)-1] {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid grpc-timeout")
		}
	}
	n, err := strconv.ParseUint(raw[:len(raw)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid grpc-timeout: %w", err)
	}
	// The protocol can express a budget longer than time.Duration. Saturate
	// safely rather than overflow to an expired or negative deadline.
	if n > uint64(math.MaxInt64/int64(unit)) {
		return time.Duration(math.MaxInt64), nil
	}
	return time.Duration(n) * unit, nil
}

func remainingGRPCTimeout(duration time.Duration) string {
	if duration <= 0 {
		return "0n"
	}
	for _, unit := range []struct {
		duration time.Duration
		suffix   string
	}{
		{time.Nanosecond, "n"}, {time.Microsecond, "u"}, {time.Millisecond, "m"},
		{time.Second, "S"}, {time.Minute, "M"}, {time.Hour, "H"},
	} {
		value := (duration-1)/unit.duration + 1
		if value <= 99999999 {
			return strconv.FormatInt(int64(value), 10) + unit.suffix
		}
	}
	panic("time.Duration exceeds gRPC timeout encoding")
}

func grpcTransport(cfg *config.Config) *http.Transport {
	tr := defaultTransport(cfg)
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetUnencryptedHTTP2(true)
	// A synchronous RPC may legitimately wait for headers until its command
	// finishes. Only the RPC's own budget should limit that wait.
	tr.ResponseHeaderTimeout = 0
	tr.DisableCompression = true
	return tr
}

func prepareGRPCResponse(resp *http.Response) error {
	// ReverseProxy may flush initial headers before it knows the body is
	// empty, losing the END_STREAM bit of a trailers-only backend reply.
	// Put the terminal fields into real trailers so the client still sees
	// them at END_STREAM, including with immediate streaming flush enabled.
	if len(resp.Header.Values("Grpc-Status")) > 0 {
		if resp.Trailer == nil {
			resp.Trailer = make(http.Header)
		}
		for _, key := range []string{"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin"} {
			if values := resp.Header.Values(key); len(values) > 0 {
				resp.Trailer[key] = values
				resp.Header.Del(key)
			}
		}
	}
	return nil
}
