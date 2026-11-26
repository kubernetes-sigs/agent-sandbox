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

// protocolKind tells the request pipeline which wire protocol the caller
// speaks. The gRPC-ish kinds share deadline handling, the h2c upstream
// transport and the explicit-port requirement; they differ only in response
// encoding and error format — a browser's fetch API cannot read HTTP
// trailers, so gRPC-Web moves the terminal status into a body frame.
type protocolKind uint8

const (
	protoHTTP protocolKind = iota
	protoGRPC
	protoGRPCWeb
	// protoGRPCWebReject marks a gRPC-Web variant this router does not
	// transcode (the base64 text encoding and the JSON codec). Rejecting
	// with a proper status frame beats forwarding a request the backend
	// cannot parse.
	protoGRPCWebReject
)

func (k protocolKind) isGRPCish() bool { return k == protoGRPC || k == protoGRPCWeb }

func (k protocolKind) String() string {
	switch k {
	case protoGRPC:
		return "grpc"
	case protoGRPCWeb:
		return "grpc-web"
	case protoGRPCWebReject:
		return "grpc-web-reject"
	default:
		return "http"
	}
}

// classifyRequest determines the wire protocol of r. Native gRPC keeps its
// HTTP/2 requirement — real gRPC clients always speak h2, and relaxing the
// gate would let malformed HTTP/1.1 requests into the gRPC pipeline. Binary
// gRPC-Web imposes no such requirement: browser fetch may arrive over
// HTTP/1.1 or HTTP/2, and the upstream hop uses h2c either way.
func classifyRequest(r *http.Request) protocolKind {
	if isGRPCRequest(r) {
		return protoGRPC
	}
	if r.Method != http.MethodPost {
		return protoHTTP
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return protoHTTP
	}
	switch mediaType {
	case contentTypeGRPCWeb, "application/grpc-web+proto":
		return protoGRPCWeb
	}
	if strings.HasPrefix(mediaType, "application/grpc-web") {
		return protoGRPCWebReject
	}
	return protoHTTP
}

// writeProxyError renders a router-level failure in the caller's protocol:
// JSON for plain HTTP, a native trailers-style response for gRPC, and a
// grpc-web trailers-only body frame for gRPC-Web. The status-code table is
// shared between the two gRPC-ish kinds so an RPC outcome never depends on
// which wire protocol the caller chose.
func writeProxyError(w http.ResponseWriter, r *http.Request, kind protocolKind, err *Error) {
	if kind == protoHTTP {
		WriteJSONError(w, err)
		return
	}
	code := codes.Internal
	message := err.Detail
	if ctxErr := grpcContextError(r.Context()); ctxErr != nil {
		code = codes.Canceled
		if ctxErr == context.DeadlineExceeded {
			code = codes.DeadlineExceeded
		}
		message = ctxErr.Error()
	} else {
		switch err.Status {
		case http.StatusBadRequest:
			code = codes.InvalidArgument
		case http.StatusUnauthorized:
			code = codes.Unauthenticated
		case http.StatusForbidden:
			code = codes.PermissionDenied
		case http.StatusBadGateway:
			code = codes.Unavailable
		case http.StatusRequestEntityTooLarge:
			code = codes.ResourceExhausted
		}
	}
	if kind == protoGRPCWeb {
		writeGRPCWebError(w, r, code, message)
		return
	}
	writeGRPCError(w, code, message)
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
	// Put the status AND application metadata into real trailers so the
	// client still sees the entire terminal block at END_STREAM, including
	// with immediate streaming flush enabled.
	if len(resp.Header.Values("Grpc-Status")) > 0 {
		if resp.Trailer == nil {
			resp.Trailer = make(http.Header)
		}
		for key, values := range resp.Header {
			if isGRPCReservedResponseHeader(key) {
				// HTTP response semantics and gRPC representation headers stay
				// in the initial block; arbitrary application metadata does not.
				continue
			}
			resp.Trailer[key] = values
			resp.Header.Del(key)
		}
	}
	return nil
}

// isGRPCReservedResponseHeader reports whether key is a known HTTP response
// field or gRPC representation header that must stay in the initial header
// block instead of moving into trailers (or a grpc-web trailer frame).
// Matches known fields exactly: application metadata can share prefixes
// such as Content-, Access-Control- or X-.
func isGRPCReservedResponseHeader(key string) bool {
	switch key {
	case "Content-Type", "Content-Length", "Trailer", "Grpc-Encoding", "Grpc-Accept-Encoding",
		"Date", "Server", "Via", "Link", "Alt-Svc",
		"Cache-Control", "Expires", "Age", "Pragma", "Warning", "Vary", "Etag", "Last-Modified",
		"Content-Encoding", "Content-Language", "Content-Location", "Content-Range", "Content-Disposition", "Accept-Ranges",
		"Allow", "Location", "Retry-After", "Set-Cookie", "Www-Authenticate", "Proxy-Authenticate",
		"Strict-Transport-Security", "Content-Security-Policy", "Content-Security-Policy-Report-Only",
		"Referrer-Policy", "Permissions-Policy", "X-Content-Type-Options", "X-Frame-Options",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Opener-Policy-Report-Only",
		"Cross-Origin-Embedder-Policy", "Cross-Origin-Embedder-Policy-Report-Only", "Cross-Origin-Resource-Policy",
		"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
		"Access-Control-Allow-Methods", "Access-Control-Allow-Headers",
		"Access-Control-Max-Age", "Access-Control-Expose-Headers":
		return true
	}
	return false
}
