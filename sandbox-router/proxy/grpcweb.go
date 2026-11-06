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
	"bytes"
	"encoding/binary"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"

	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// gRPC-Web framing shares the Length-Prefixed-Message format with native
// gRPC — one flag byte, four big-endian length bytes, payload — so message
// frames cross the proxy untouched. The only frame this package builds is
// the trailer frame (flag 0x80), which carries the terminal status that a
// native gRPC response would deliver as HTTP trailers. Browsers cannot read
// HTTP trailers, which is exactly why the grpc-web protocol moves them into
// the body.
const (
	grpcWebTrailerFlag = 0x80
	contentTypeGRPCWeb = "application/grpc-web"
)

// encodeTrailerFrame wraps payload in the 5-byte grpc-web frame header with
// the trailer flag set. Trailer frames are never compressed, independent of
// any grpc-encoding negotiated for message frames.
func encodeTrailerFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = grpcWebTrailerFlag
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// trailerFramePayload serializes response trailers into the grpc-web trailer
// frame body: "key:value\r\n" lines with lower-case keys, grpc-status first
// and grpc-message second (clients may stop parsing after the status), then
// any remaining metadata in sorted order so the output is deterministic.
// Values pass through as the backend sent them — the gRPC wire format
// already percent-encodes grpc-message.
func trailerFramePayload(h http.Header) []byte {
	if len(h) == 0 {
		return nil
	}
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, strings.ToLower(key))
	}
	slices.Sort(keys)
	ordered := make([]string, 0, len(keys))
	for _, special := range []string{"grpc-status", "grpc-message"} {
		if h.Values(http.CanonicalHeaderKey(special)) != nil {
			ordered = append(ordered, special)
		}
	}
	for _, key := range keys {
		if key != "grpc-status" && key != "grpc-message" {
			ordered = append(ordered, key)
		}
	}

	var buf strings.Builder
	for _, key := range ordered {
		for _, value := range h.Values(http.CanonicalHeaderKey(key)) {
			buf.WriteString(key)
			buf.WriteByte(':')
			buf.WriteString(value)
			buf.WriteString("\r\n")
		}
	}
	return []byte(buf.String())
}

// trailerFrameFromHeaders builds the complete terminal frame for a grpc-web
// response body from native gRPC trailers.
func trailerFrameFromHeaders(h http.Header) []byte {
	return encodeTrailerFrame(trailerFramePayload(h))
}

// writeGRPCWebError answers a request classified as gRPC-Web with a
// trailers-only error response: HTTP 200 carrying a single trailer frame.
// The 200 matches Envoy's grpc-web filter and the shape browser clients
// parse most reliably — the RPC-level outcome travels in grpc-status, never
// in the HTTP status. The message is percent-encoded per the gRPC spec.
func writeGRPCWebError(w http.ResponseWriter, r *http.Request, code codes.Code, message string) {
	payload := "grpc-status:" + strconv.Itoa(int(code)) + "\r\n" +
		"grpc-message:" + url.PathEscape(message) + "\r\n"
	frame := encodeTrailerFrame([]byte(payload))
	observability.RecordGRPCStatus(r.Context(), strconv.Itoa(int(code)))
	w.Header().Set("Content-Type", contentTypeGRPCWeb)
	w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
}

// grpcWebPreflightRequestHeaders is the closed set of request headers a
// gRPC-Web call needs: the wire content type and deadline, the routing
// headers, the credential the router consumes, and the tracing/agent
// identification headers the SDKs attach. A preflight echoing only this
// set is how CORS stays scoped to gRPC-Web — a plain-REST browser call
// asking for anything else gets an Allow-Headers list its real request
// cannot satisfy, and the browser blocks it.
var grpcWebPreflightRequestHeaders = map[string]struct{}{
	"content-type":        {},
	"grpc-timeout":        {},
	"x-sandbox-id":        {},
	"x-sandbox-uid":       {},
	"x-sandbox-namespace": {},
	"x-sandbox-port":      {},
	"x-sandbox-pod-ip":    {},
	"authorization":       {},
	"x-user-agent":        {},
	"traceparent":         {},
	// The official grpc-web browser client marks every request with
	// X-Grpc-Web: 1; without it in the set, that client's preflight fails.
	"x-grpc-web": {},
}

// maybeHandlePreflight answers a CORS preflight from an origin explicitly
// listed in --grpc-web-allowed-origins before routing: the request carries
// no X-Sandbox-* values (the browser only lists their names in
// Access-Control-Request-Headers), so it can be neither routed nor
// authorized. Every other OPTIONS request — including same-origin ones,
// which browsers never actually preflight — falls through to the normal
// proxy path, so deployments that do not configure the flag see exactly
// the OPTIONS behavior they had before gRPC-Web support existed.
func (h *Handler) maybeHandlePreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	if !originInList(origin, h.cfg.GRPCWebAllowedOrigins) {
		return false
	}
	hdr := w.Header()
	hdr.Set("Access-Control-Allow-Origin", origin)
	// Credentials mode must never pair with a wildcard origin; echo the
	// exact origin so cookie-authenticated gRPC-Web deployments work.
	hdr.Set("Access-Control-Allow-Credentials", "true")
	hdr.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if allowed := intersectAllowedRequestHeaders(r.Header.Get("Access-Control-Request-Headers")); allowed != "" {
		hdr.Set("Access-Control-Allow-Headers", allowed)
	}
	hdr.Set("Access-Control-Max-Age", "600")
	hdr.Add("Vary", "Origin")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// intersectAllowedRequestHeaders filters the comma-separated
// Access-Control-Request-Headers list down to the gRPC-Web set,
// preserving the caller's order.
func intersectAllowedRequestHeaders(requested string) string {
	var kept []string
	for name := range strings.SplitSeq(requested, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := grpcWebPreflightRequestHeaders[name]; ok {
			kept = append(kept, name)
		}
	}
	return strings.Join(kept, ",")
}

// originInList reports whether origin matches an explicit allowlist entry.
// Unlike isAllowedOrigin, same-origin does not pass: browsers preflight
// cross-origin requests only, so answering a same-origin OPTIONS would
// divert traffic from sandboxes that handle it themselves without helping
// any browser caller.
func originInList(origin string, allowed []string) bool {
	norm := normalizeOrigin(origin)
	for _, a := range allowed {
		if strings.EqualFold(normalizeOrigin(a), norm) {
			return true
		}
	}
	return false
}

// maybeSetCORSHeaders stamps the CORS response headers a browser needs to
// read a gRPC-Web response. It runs before routing and authorization so
// error responses — the denial itself especially — are readable too. A
// disallowed origin is not rejected here: standard CORS semantics let the
// server execute the request while the browser blocks the read, and
// refusing outright would break non-browser clients that happen to send
// Origin. The cookie-credential origin guard is separate and unchanged.
func (h *Handler) maybeSetCORSHeaders(w http.ResponseWriter, r *http.Request, kind protocolKind) {
	if kind != protoGRPCWeb && kind != protoGRPCWebReject {
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if !isAllowedOrigin(origin, requestOrigin(r, h.cfg.AuthzTrustForwardedProto), h.cfg.GRPCWebAllowedOrigins) {
		return
	}
	hdr := w.Header()
	hdr.Set("Access-Control-Allow-Origin", origin)
	hdr.Set("Access-Control-Allow-Credentials", "true")
	hdr.Add("Vary", "Origin")
}

// rewriteGRPCWebContentType maps the inbound gRPC-Web content type onto
// the native gRPC type the backend expects, preserving any codec suffix
// (application/grpc-web+proto → application/grpc+proto). Message framing
// is identical between the two protocols, so this header is the only
// request-side translation. The value is parsed rather than matched
// verbatim: classification accepts media-type parameters, and grpc-go
// rejects a content type that carries any — the rewritten value must be
// exactly application/grpc[+codec].
func rewriteGRPCWebContentType(h http.Header) {
	raw := h.Get("Content-Type")
	if raw == "" {
		return
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return
	}
	if codec, extended := strings.CutPrefix(mediaType, "application/grpc-web+"); extended {
		h.Set("Content-Type", "application/grpc+"+codec)
		return
	}
	if mediaType == contentTypeGRPCWeb {
		h.Set("Content-Type", "application/grpc")
	}
}

// prepareGRPCWebResponse adapts a native gRPC backend response for a
// gRPC-Web caller. Browsers cannot read HTTP trailers, so the terminal
// status must travel inside the body as a trailer frame — either by
// synthesizing a body for a trailers-only response, or by appending the
// frame when the stream ends. record receives the terminal status exactly
// once for the observability Labels; it may be nil in tests.
func prepareGRPCWebResponse(resp *http.Response, record func(string)) error {
	// The router owns CORS for gRPC-Web; a backend echo would duplicate
	// the header the handler already stamped on the client response.
	resp.Header.Del("Access-Control-Allow-Origin")
	resp.Header.Del("Access-Control-Allow-Credentials")

	if len(resp.Header.Values("Grpc-Status")) > 0 {
		// Trailers-only response: the whole terminal block sits in the
		// initial headers and the body is empty. Move status, message and
		// any application metadata into a synthesized trailer frame.
		status := resp.Header.Get("Grpc-Status")
		metadata := http.Header{}
		for key, values := range resp.Header {
			if isGRPCReservedResponseHeader(key) {
				continue
			}
			metadata[key] = values
			resp.Header.Del(key)
		}
		frame := trailerFrameFromHeaders(metadata)
		resp.Body = io.NopCloser(bytes.NewReader(frame))
		resp.ContentLength = int64(len(frame))
		// The backend's Content-Length header (when it sent one) describes
		// the empty native body, not the synthesized frame; leaving it
		// would misframe the response for HTTP/1.1 clients.
		resp.Header.Set("Content-Length", strconv.Itoa(len(frame)))
		resp.Trailer = nil
		if record != nil {
			record(status)
		}
	} else {
		resp.Body = newGRPCWebBodyTransformer(resp, record)
		// A backend-supplied Content-Length covers the message bytes
		// only; the appended trailer frame makes the body longer, and an
		// HTTP/1.1 writer enforcing the stale value would abort with
		// http.ErrContentLength. The streamed length is unknown here, so
		// let the transport frame it.
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}
	setGRPCWebResponseContentType(resp.Header)
	return nil
}

// setGRPCWebResponseContentType maps the backend's native gRPC content
// type onto its gRPC-Web counterpart, preserving any codec suffix.
func setGRPCWebResponseContentType(h http.Header) {
	if codec, extended := strings.CutPrefix(h.Get("Content-Type"), "application/grpc+"); extended {
		h.Set("Content-Type", contentTypeGRPCWeb+"+"+codec)
		return
	}
	h.Set("Content-Type", contentTypeGRPCWeb)
}

// grpcWebBodyTransformer streams backend message frames through untouched
// — the framing is identical in both protocols, including compressed
// frames — and appends the terminal trailer frame once the stream ends.
// It holds the whole response, not a trailer-map reference: a gRPC backend
// sends no Trailer announcement, so resp.Trailer starts nil and the h2
// client installs a fresh map through the response pointer at EOF
// (copyTrailers) — exactly when the terminal frame is due. Reading the
// map captured at prepare time would therefore always see empty trailers.
type grpcWebBodyTransformer struct {
	src      io.ReadCloser
	resp     *http.Response
	record   func(string)
	tail     []byte // synthesized terminal frame not yet served
	done     bool
	recorded bool
}

func newGRPCWebBodyTransformer(resp *http.Response, record func(string)) *grpcWebBodyTransformer {
	return &grpcWebBodyTransformer{src: resp.Body, resp: resp, record: record}
}

func (t *grpcWebBodyTransformer) Read(p []byte) (int, error) {
	for {
		if len(t.tail) > 0 {
			n := copy(p, t.tail)
			t.tail = t.tail[n:]
			return n, nil
		}
		if t.done {
			return 0, io.EOF
		}
		n, err := t.src.Read(p)
		if n > 0 {
			// Any error travels with the next Read; message frames must
			// reach the client byte-identical first.
			return n, nil
		}
		if err == nil {
			continue
		}
		t.done = true
		if err == io.EOF {
			t.tail = t.terminalFrame()
		} else {
			// A grpc-web client can only learn the RPC outcome from a
			// trailer frame; ending the body without one leaves it
			// hanging, so a broken upstream stream becomes Unavailable.
			t.tail = t.synthesizedFrame("14", "upstream stream terminated")
		}
		if len(t.tail) == 0 {
			return 0, io.EOF
		}
	}
}

// terminalFrame builds the frame from the trailers the backend ended the
// stream with. A clean EOF without a grpc-status is a protocol violation
// on the backend side; report Internal rather than leaving the client
// without an outcome.
func (t *grpcWebBodyTransformer) terminalFrame() []byte {
	if status := t.resp.Trailer.Get("Grpc-Status"); status != "" {
		t.recordOnce(status)
		return trailerFrameFromHeaders(t.resp.Trailer)
	}
	return t.synthesizedFrame("13", "backend closed stream without status")
}

func (t *grpcWebBodyTransformer) synthesizedFrame(code, message string) []byte {
	t.recordOnce(code)
	return encodeTrailerFrame([]byte("grpc-status:" + code + "\r\ngrpc-message:" + message + "\r\n"))
}

func (t *grpcWebBodyTransformer) recordOnce(status string) {
	if !t.recorded {
		t.recorded = true
		if t.record != nil {
			t.record(status)
		}
	}
}

func (t *grpcWebBodyTransformer) Close() error { return t.src.Close() }
