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

package extproc

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/internal/cache"
)

// stubCache satisfies Lookup with a fixed in-memory map. Lets tests drive
// hit / miss behavior without spinning up an informer.
type stubCache map[types.UID]cache.Entry

func (s stubCache) Get(uid types.UID) (cache.Entry, bool) {
	e, ok := s[uid]
	return e, ok
}

// newServer builds a Server with a stub cache. Returns the server and
// the stub so individual tests can mutate the map.
func newServer(t *testing.T) (*Server, stubCache) {
	t.Helper()
	stub := stubCache{}
	s, err := NewServer(Options{
		Cache:         stub,
		ClusterDomain: "cluster.local",
		Log:           logr.Discard(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, stub
}

// hdrs builds an Envoy HttpHeaders message from a key→value map. Lower
// cases keys to match Envoy's normalization.
func hdrs(kv map[string]string) *extprocv3.HttpHeaders {
	hm := &corev3.HeaderMap{}
	for k, v := range kv {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{
			Key:      strings.ToLower(k),
			RawValue: []byte(v),
		})
	}
	return &extprocv3.HttpHeaders{Headers: hm}
}

// reqHeaders wraps hdrs into a ProcessingRequest of the RequestHeaders
// phase, which is the only phase our handler actually does work on.
func reqHeaders(kv map[string]string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: hdrs(kv),
		},
	}
}

// originalDstHostFrom extracts the value of the x-envoy-original-dst-host
// header from a successful HeadersResponse, returning ("", false) if the
// response is an ImmediateResponse or the mutation isn't present.
func originalDstHostFrom(resp *extprocv3.ProcessingResponse) (string, bool) {
	hr := resp.GetRequestHeaders()
	if hr == nil {
		return "", false
	}
	for _, h := range hr.Response.HeaderMutation.SetHeaders {
		if h.Header.Key == HeaderOriginalDstHost {
			return string(h.Header.RawValue), true
		}
	}
	return "", false
}

func TestHandle_CacheHitSetsOriginalDstFromPodIP(t *testing.T) {
	s, stub := newServer(t)
	uid := types.UID("11111111-2222-3333-4444-555555555555")
	stub[uid] = cache.Entry{PodIP: "10.0.0.7", SandboxName: "alpha", Namespace: "tenant-a"}

	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:        "alpha",
		HeaderSandboxUID:       string(uid),
		HeaderSandboxNamespace: "tenant-a",
		HeaderSandboxPort:      "9090",
	}))

	host, ok := originalDstHostFrom(resp)
	if !ok {
		t.Fatalf("expected header mutation; got: %+v", resp)
	}
	if host != "10.0.0.7:9090" {
		t.Errorf("original-dst-host: got %q want %q", host, "10.0.0.7:9090")
	}
	if !resp.GetRequestHeaders().Response.ClearRouteCache {
		t.Errorf("ClearRouteCache must be set so Envoy re-routes after our mutation")
	}
}

func TestHandle_CacheMissFallsBackToDNS(t *testing.T) {
	s, _ := newServer(t)

	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:        "ghost",
		HeaderSandboxUID:       "no-such-uid",
		HeaderSandboxNamespace: "tenant-b",
		HeaderSandboxPort:      "8888",
	}))

	host, ok := originalDstHostFrom(resp)
	if !ok {
		t.Fatalf("expected mutation, got: %+v", resp)
	}
	want := "ghost.tenant-b.svc.cluster.local:8888"
	if host != want {
		t.Errorf("DNS form: got %q want %q", host, want)
	}
}

func TestHandle_NoUIDStillRoutesViaDNS(t *testing.T) {
	s, _ := newServer(t)

	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:        "alpha",
		HeaderSandboxNamespace: "default",
	}))

	host, ok := originalDstHostFrom(resp)
	if !ok {
		t.Fatalf("expected mutation, got: %+v", resp)
	}
	if host != "alpha.default.svc.cluster.local:8888" {
		t.Errorf("DNS form with default port: got %q", host)
	}
}

func TestHandle_DefaultsAppliedForNamespaceAndPort(t *testing.T) {
	s, _ := newServer(t)

	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID: "alpha",
	}))
	host, _ := originalDstHostFrom(resp)
	if host != "alpha.default.svc.cluster.local:8888" {
		t.Errorf("defaults: got %q", host)
	}
}

func TestHandle_MissingSandboxIDReturnsImmediate400(t *testing.T) {
	s, _ := newServer(t)
	resp := s.handle(context.Background(), reqHeaders(map[string]string{}))

	ir := resp.GetImmediateResponse()
	if ir == nil {
		t.Fatalf("expected ImmediateResponse, got: %+v", resp)
	}
	if ir.Status.Code != 400 {
		t.Errorf("status: got %d want 400", ir.Status.Code)
	}
	if !strings.Contains(string(ir.Body), "X-Sandbox-ID") {
		t.Errorf("body should mention X-Sandbox-ID: %s", ir.Body)
	}
}

func TestHandle_InvalidNamespaceRejected(t *testing.T) {
	s, _ := newServer(t)
	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:        "alpha",
		HeaderSandboxNamespace: "bad namespace!",
	}))
	ir := resp.GetImmediateResponse()
	if ir == nil || ir.Status.Code != 400 {
		t.Fatalf("expected 400 immediate; got: %+v", resp)
	}
	if !strings.Contains(string(ir.Body), "namespace") {
		t.Errorf("body should mention namespace: %s", ir.Body)
	}
}

func TestHandle_InvalidPortRejected(t *testing.T) {
	// Port must fall in [1, 65535]. Anything else gets rejected with a
	// 400 immediate before we hand the value to net.JoinHostPort, so a
	// bogus value can't ride along into x-envoy-original-dst-host and
	// surface as a less actionable Envoy error.
	cases := []struct {
		name string
		port string
	}{
		{"non-numeric", "abc"},
		{"empty-after-trim", " "},
		{"zero", "0"},
		{"negative", "-1"},
		{"way negative", "-99999"},
		{"just over 65535", "65536"},
		{"big number", "100000"},
		{"max int32", "2147483647"},
	}
	s, _ := newServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.handle(context.Background(), reqHeaders(map[string]string{
				HeaderSandboxID:   "alpha",
				HeaderSandboxPort: tc.port,
			}))
			ir := resp.GetImmediateResponse()
			if ir == nil || ir.Status.Code != 400 {
				t.Fatalf("port=%q: expected 400 immediate; got: %+v", tc.port, resp)
			}
			if !strings.Contains(string(ir.Body), "port") {
				t.Errorf("port=%q: body should mention port: %s", tc.port, ir.Body)
			}
		})
	}
}

func TestHandle_PortBoundariesAccepted(t *testing.T) {
	// The smallest and largest legal TCP ports both make it through.
	s, stub := newServer(t)
	uid := types.UID("boundary-uid")
	stub[uid] = cache.Entry{PodIP: "10.0.0.1"}
	for _, port := range []string{"1", "65535"} {
		t.Run(port, func(t *testing.T) {
			resp := s.handle(context.Background(), reqHeaders(map[string]string{
				HeaderSandboxID:   "alpha",
				HeaderSandboxUID:  string(uid),
				HeaderSandboxPort: port,
			}))
			if ir := resp.GetImmediateResponse(); ir != nil {
				t.Fatalf("port %s: unexpected immediate %d / %s", port, ir.Status.Code, ir.Body)
			}
			rh := resp.GetRequestHeaders()
			if rh == nil {
				t.Fatalf("port %s: expected RequestHeaders mutation; got %+v", port, resp)
			}
			want := "10.0.0.1:" + port
			got := ""
			for _, h := range rh.Response.HeaderMutation.SetHeaders {
				if h.Header.Key == HeaderOriginalDstHost {
					got = string(h.Header.RawValue)
				}
			}
			if got != want {
				t.Fatalf("port %s: dst host got %q want %q", port, got, want)
			}
		})
	}
}

// TestHandle_NonRequestHeadersPhasePassesThrough confirms we don't fail
// the stream when Envoy is misconfigured to call us for body/response
// phases — we just CONTINUE.
// TestHandle_PhaseMatchedContinue exercises the protocol-correctness
// guarantee: the ProcessingResponse oneof variant must match the
// ProcessingRequest oneof variant for every phase Envoy can send.
// Sending the wrong variant (e.g. a RequestHeaders envelope in reply
// to a RequestBody request) is a protocol violation that aborts the
// ext_proc stream and therefore the user's request.
func TestHandle_PhaseMatchedContinue(t *testing.T) {
	s, _ := newServer(t)

	cases := []struct {
		name string
		req  *extprocv3.ProcessingRequest
		// pick returns the matching variant from the response; nil
		// means the test failed the variant check.
		pick func(*extprocv3.ProcessingResponse) any
	}{
		{
			name: "ResponseHeaders → ResponseHeaders",
			req: &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_ResponseHeaders{ResponseHeaders: hdrs(nil)},
			},
			pick: func(r *extprocv3.ProcessingResponse) any { return r.GetResponseHeaders() },
		},
		{
			name: "RequestBody → RequestBody",
			req: &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: &extprocv3.HttpBody{}},
			},
			pick: func(r *extprocv3.ProcessingResponse) any { return r.GetRequestBody() },
		},
		{
			name: "ResponseBody → ResponseBody",
			req: &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_ResponseBody{ResponseBody: &extprocv3.HttpBody{}},
			},
			pick: func(r *extprocv3.ProcessingResponse) any { return r.GetResponseBody() },
		},
		{
			name: "RequestTrailers → RequestTrailers",
			req: &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_RequestTrailers{RequestTrailers: &extprocv3.HttpTrailers{}},
			},
			pick: func(r *extprocv3.ProcessingResponse) any { return r.GetRequestTrailers() },
		},
		{
			name: "ResponseTrailers → ResponseTrailers",
			req: &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_ResponseTrailers{ResponseTrailers: &extprocv3.HttpTrailers{}},
			},
			pick: func(r *extprocv3.ProcessingResponse) any { return r.GetResponseTrailers() },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.handle(context.Background(), tc.req)
			if resp == nil {
				t.Fatalf("nil response for known phase")
			}
			// Guard: a wrong-variant response would have GetRequestHeaders
			// non-nil for, say, the RequestBody case. Assert the matching
			// variant is set AND that no other variant leaked in.
			matched := tc.pick(resp)
			if matched == nil || reflect.ValueOf(matched).IsNil() {
				t.Fatalf("phase-matched variant missing on %T; got %+v", tc.req.Request, resp.Response)
			}
		})
	}
}

// TestHandle_UnknownPhaseReturnsNil documents the contract for unknown
// (future) ProcessingRequest oneof variants: handle returns nil and the
// Process loop drops the message rather than sending a wrong-phase
// reply that would abort the stream.
func TestHandle_UnknownPhaseReturnsNil(t *testing.T) {
	if got := continueFor(&extprocv3.ProcessingRequest{Request: nil}); got != nil {
		t.Fatalf("unknown oneof should yield nil ProcessingResponse, got %+v", got)
	}
}

func TestValidNamespace(t *testing.T) {
	cases := map[string]bool{
		"default": true,
		"prod":    true,
		"my-ns":   true,
		"my-ns-1": true,
		"MY-NS":   true,
		"a":       true,
		"":        false,
		"-":       false,
		"---":     false,
		"my_ns":   false,
		"my.ns":   false,
		" ns":     false,
		"ns ":     false,
		"bad!":    false,
	}
	for in, want := range cases {
		if got := validNamespace(in); got != want {
			t.Errorf("validNamespace(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestReadHeaders_AcceptsBothRawAndStringValues(t *testing.T) {
	hm := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: HeaderSandboxID, RawValue: []byte("from-raw")},
		{Key: HeaderSandboxNamespace, Value: "from-string"},
	}}
	r := readHeaders(hm)
	if r.id != "from-raw" {
		t.Errorf("RawValue not honored: got %q", r.id)
	}
	if r.namespace != "from-string" {
		t.Errorf("legacy Value not honored: got %q", r.namespace)
	}
}

func TestJoinHostPort(t *testing.T) {
	cases := []struct {
		name string
		host string
		port int
		want string
	}{
		{"ipv4", "10.0.0.1", 8888, "10.0.0.1:8888"},
		{"dns", "alpha.default.svc.cluster.local", 8888, "alpha.default.svc.cluster.local:8888"},
		// IPv6 literals must be bracketed per RFC 3986, otherwise the
		// trailing colon-port is ambiguous with the address itself. Pod
		// IPs on dual-stack / IPv6-only clusters surface as bare IPv6
		// strings in Pod.Status.PodIP.
		{"ipv6 loopback", "::1", 8888, "[::1]:8888"},
		{"ipv6 link-local", "fe80::1", 9090, "[fe80::1]:9090"},
		{"ipv6 full", "2001:db8::1", 8888, "[2001:db8::1]:8888"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinHostPort(tc.host, tc.port); got != tc.want {
				t.Errorf("joinHostPort(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestResolve_IPv6PodIPGetsBracketed(t *testing.T) {
	s, stub := newServer(t)
	uid := types.UID("v6-uid")
	stub[uid] = cache.Entry{PodIP: "2001:db8::42"}

	target, source := s.resolve(request{
		id: "alpha", uid: string(uid), namespace: "default", port: 8888,
	})
	if target != "[2001:db8::42]:8888" {
		t.Errorf("target: got %q want [2001:db8::42]:8888", target)
	}
	if source != "cache" {
		t.Errorf("source: got %q want cache", source)
	}
}

func TestResolve_PrefersCacheOverDNS(t *testing.T) {
	s, stub := newServer(t)
	uid := types.UID("aaaa-bbbb")
	stub[uid] = cache.Entry{PodIP: "10.20.30.40"}

	target, source := s.resolve(request{
		id: "alpha", uid: string(uid), namespace: "default", port: 8888,
	})
	if target != "10.20.30.40:8888" {
		t.Errorf("target: got %q", target)
	}
	if source != "cache" {
		t.Errorf("source: got %q want cache", source)
	}
}

// removeHeadersFrom extracts the RemoveHeaders list from a successful
// HeadersResponse.
func removeHeadersFrom(resp *extprocv3.ProcessingResponse) []string {
	hr := resp.GetRequestHeaders()
	if hr == nil || hr.Response == nil || hr.Response.HeaderMutation == nil {
		return nil
	}
	return hr.Response.HeaderMutation.RemoveHeaders
}

func TestHandle_StripsOriginOnUpgrade(t *testing.T) {
	s, stub := newServer(t)
	uid := types.UID("ws-uid")
	stub[uid] = cache.Entry{PodIP: "10.0.0.7"}

	// True WebSocket upgrade: BOTH Connection: Upgrade AND a non-empty
	// Upgrade header. Envoy normalizes header keys to lowercase, so
	// "origin" — not "Origin" — is the key we must remove.
	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:  "alpha",
		HeaderSandboxUID: string(uid),
		"Connection":     "keep-alive, Upgrade",
		"Upgrade":        "websocket",
		"Origin":         "https://router.example.com",
	}))
	removed := removeHeadersFrom(resp)
	found := false
	for _, h := range removed {
		if h == "origin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected RemoveHeaders to contain \"origin\"; got %v", removed)
	}
	// And the dst-host mutation must still be there — we're adding,
	// not replacing the existing mutation.
	if dst, ok := originalDstHostFrom(resp); !ok || dst != "10.0.0.7:8888" {
		t.Fatalf("dst-host mutation lost: got %q (ok=%v)", dst, ok)
	}
}

func TestHandle_NonUpgradePreservesOrigin(t *testing.T) {
	// Guard against the strip leaking into normal HTTP — would break
	// CORS preflights and any backend that uses Origin on
	// non-WebSocket traffic.
	s, stub := newServer(t)
	uid := types.UID("plain-uid")
	stub[uid] = cache.Entry{PodIP: "10.0.0.8"}

	resp := s.handle(context.Background(), reqHeaders(map[string]string{
		HeaderSandboxID:  "alpha",
		HeaderSandboxUID: string(uid),
		"Origin":         "https://client.example.com",
		// no Connection / Upgrade headers
	}))
	for _, h := range removeHeadersFrom(resp) {
		if h == "origin" {
			t.Fatalf("Origin must not be stripped on non-upgrade; got RemoveHeaders=%v", removeHeadersFrom(resp))
		}
	}
}

func TestReadHeaders_UpgradeDetection(t *testing.T) {
	// Locks in the predicate: an upgrade is recognized iff BOTH
	// Connection contains an upgrade token AND Upgrade is non-empty.
	// Matches httputil.ReverseProxy's internal check on the
	// from-scratch router so behavior is consistent across PRs.
	cases := []struct {
		name string
		hdrs map[string]string
		want bool
	}{
		{"both present", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}, true},
		{"connection list with upgrade token", map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "websocket"}, true},
		{"case-insensitive scheme", map[string]string{"Connection": "upgrade", "Upgrade": "WebSocket"}, true},
		{"missing Connection", map[string]string{"Upgrade": "websocket"}, false},
		{"missing Upgrade", map[string]string{"Connection": "Upgrade"}, false},
		{"connection without upgrade token", map[string]string{"Connection": "keep-alive", "Upgrade": "websocket"}, false},
		{"empty Upgrade value", map[string]string{"Connection": "Upgrade", "Upgrade": ""}, false},
		{"neither", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readHeaders(hdrs(tc.hdrs).Headers).upgrade
			if got != tc.want {
				t.Fatalf("got upgrade=%v want %v (hdrs=%v)", got, tc.want, tc.hdrs)
			}
		})
	}
}
