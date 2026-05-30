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

// Package extproc implements the Envoy ext_proc v3 ExternalProcessor
// service for the sandbox-router. The handler is intentionally narrow: it
// only observes request headers, and its only side effect is setting the
// `x-envoy-original-dst-host` header so Envoy's ORIGINAL_DST cluster
// dispatches the request to the right sandbox Pod.
package extproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/internal/cache"
)

// Header names the handler consumes. Envoy normalizes to lowercase per
// HTTP/2 spec, so we match lowercase exactly.
const (
	HeaderSandboxID        = "x-sandbox-id"
	HeaderSandboxUID       = "x-sandbox-uid"
	HeaderSandboxNamespace = "x-sandbox-namespace"
	HeaderSandboxPort      = "x-sandbox-port"

	// HeaderOriginalDstHost is the header Envoy's ORIGINAL_DST cluster
	// reads to select the upstream when `use_http_header: true`. We set
	// it to "<ip-or-host>:<port>" on every accepted request.
	HeaderOriginalDstHost = "x-envoy-original-dst-host"
)

const (
	defaultSandboxPort      = 8888
	defaultSandboxNamespace = "default"
)

// Lookup is the slice of the cache the handler depends on. Defined as an
// interface so tests can inject a stub without spinning a real informer.
type Lookup interface {
	Get(uid types.UID) (cache.Entry, bool)
}

// Server implements envoy.service.ext_proc.v3.ExternalProcessorServer.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer

	cache         Lookup
	clusterDomain string
	log           logr.Logger
}

// Options configure a new Server. ClusterDomain is the K8s cluster DNS
// suffix used for the DNS-form fallback (defaults to "cluster.local").
type Options struct {
	Cache         Lookup
	ClusterDomain string
	Log           logr.Logger
}

// NewServer constructs a Server. Cache is required; ClusterDomain
// defaults if empty.
func NewServer(o Options) (*Server, error) {
	if o.Cache == nil {
		return nil, errors.New("extproc: Cache is required")
	}
	if o.ClusterDomain == "" {
		o.ClusterDomain = "cluster.local"
	}
	return &Server{
		cache:         o.Cache,
		clusterDomain: o.ClusterDomain,
		log:           o.Log,
	}, nil
}

// Process is the bidirectional stream Envoy opens per request. We only
// inspect REQUEST_HEADERS; other phases come back as immediate
// CONTINUE responses so Envoy can move on without further callbacks.
// The Envoy ProcessingMode config should be set to skip the other phases
// so we don't waste round-trips, but defending here keeps the server
// correct under misconfiguration.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp := s.handle(stream.Context(), req)
		if resp == nil {
			// Unknown ProcessingRequest phase (newer Envoy variant we
			// don't recognize). Skipping the Send keeps the stream open
			// and lets Envoy decide what to do; sending a wrong-phase
			// reply would abort it.
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// handle is the side-effect-free part of Process. Returns the response to
// send back to Envoy. Header decisions never need to abort the stream —
// validation failures come back as ImmediateResponse, not stream errors.
//
// Phases other than REQUEST_HEADERS return a phase-matched CONTINUE: the
// ext_proc spec requires the ProcessingResponse oneof variant to match
// the ProcessingRequest oneof variant (a ResponseBody request answered
// with a RequestHeaders envelope is a protocol violation that aborts
// the stream). The Envoy processing_mode config should have suppressed
// these phases at the source, but we stay protocol-correct in case it
// drifts.
func (s *Server) handle(ctx context.Context, req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	switch r := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		return s.onRequestHeaders(ctx, r.RequestHeaders)
	default:
		return continueFor(req)
	}
}

// continueFor returns a CONTINUE-equivalent ProcessingResponse whose
// oneof variant matches req's. Used as the no-op reply for off-mode
// phases. TrailersResponse carries no CommonResponse field (its only
// payload is an optional HeaderMutation), so its CONTINUE-equivalent
// is an empty TrailersResponse — Envoy treats absent HeaderMutation as
// "no change, proceed". Returns nil for unknown oneof variants (newer
// Envoy phases we don't know about); Process drops nil responses
// rather than sending a phase-mismatched envelope.
func continueFor(req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	common := &extprocv3.CommonResponse{Status: extprocv3.CommonResponse_CONTINUE}

	switch req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{Response: common},
			},
		}
	case *extprocv3.ProcessingRequest_ResponseHeaders:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{Response: common},
			},
		}
	case *extprocv3.ProcessingRequest_RequestBody:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{Response: common},
			},
		}
	case *extprocv3.ProcessingRequest_ResponseBody:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{Response: common},
			},
		}
	case *extprocv3.ProcessingRequest_RequestTrailers:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestTrailers{
				RequestTrailers: &extprocv3.TrailersResponse{},
			},
		}
	case *extprocv3.ProcessingRequest_ResponseTrailers:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseTrailers{
				ResponseTrailers: &extprocv3.TrailersResponse{},
			},
		}
	default:
		return nil
	}
}

// onRequestHeaders parses the routing headers, looks up the target, and
// returns either a HeadersResponse mutating x-envoy-original-dst-host or
// an ImmediateResponse on validation failure.
func (s *Server) onRequestHeaders(_ context.Context, hdrs *extprocv3.HttpHeaders) *extprocv3.ProcessingResponse {
	r := readHeaders(hdrs.Headers)

	if r.id == "" {
		return immediate(400, `{"detail":"X-Sandbox-ID header is required."}`)
	}
	if !validNamespace(r.namespace) {
		return immediate(400, `{"detail":"Invalid namespace format."}`)
	}
	// TCP port range is [1, 65535]. Reject anything outside it (and the
	// 0 sentinel readHeaders sets on non-numeric input) before we hand
	// the value to net.JoinHostPort — an out-of-range port would
	// produce a syntactically valid but semantically junk
	// x-envoy-original-dst-host (e.g. "10.0.0.1:99999") that Envoy
	// would reject downstream with a less actionable error.
	if r.port < 1 || r.port > 65535 {
		return immediate(400, `{"detail":"Invalid port format."}`)
	}

	target, source := s.resolve(r)
	if target == "" {
		// Shouldn't happen — resolve only returns "" when both cache miss
		// AND we can't construct a DNS form, which requires no id (already
		// rejected above). Defensive.
		return immediate(500, `{"detail":"unable to resolve sandbox target"}`)
	}

	s.log.V(2).Info("routing",
		"sandbox_id", r.id,
		"sandbox_uid", r.uid,
		"namespace", r.namespace,
		"port", r.port,
		"target", target,
		"source", source,
	)

	mutation := &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{{
			Header: &corev3.HeaderValue{
				Key:      HeaderOriginalDstHost,
				RawValue: []byte(target),
			},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}},
	}
	if r.upgrade {
		// Strip Origin on upgrade so WebSocket backends that validate
		// Origin == Host for CSRF protection (vscode-server, Jupyter,
		// etc.) don't reject the upgrade with a 1006 close because the
		// client-supplied Origin (router's external hostname) doesn't
		// match the backend's own Host. Envoy normalizes header keys
		// to lowercase, so "origin" — not "Origin" — is the key to
		// remove. Mirrors the equivalent fix on the from-scratch Go
		// router (PR #838, commit d04bfb5).
		mutation.RemoveHeaders = append(mutation.RemoveHeaders, "origin")
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					Status:         extprocv3.CommonResponse_CONTINUE,
					HeaderMutation: mutation,
					// Route is recomputed after our mutation so Envoy picks
					// the ORIGINAL_DST cluster with the new dst host.
					ClearRouteCache: true,
				},
			},
		},
	}
}

// resolve picks the upstream host:port for the request. Returns the
// target and a tag describing how it was resolved (for logs/metrics).
//
// Order: cache hit by UID wins (fast + secure); DNS form is the fallback
// (works without UID, slower, less secure).
func (s *Server) resolve(r request) (target, source string) {
	if r.uid != "" {
		if e, ok := s.cache.Get(types.UID(r.uid)); ok {
			return joinHostPort(e.PodIP, r.port), "cache"
		}
	}
	// DNS form: <id>.<ns>.svc.<cluster-domain>:<port>
	host := r.id + "." + r.namespace + ".svc." + s.clusterDomain
	return joinHostPort(host, r.port), "dns"
}

// request is the parsed routing input.
type request struct {
	id        string
	uid       string
	namespace string
	port      int
	// upgrade is true when the inbound request asked to switch
	// protocols (Connection: Upgrade + Upgrade: <proto>, typically
	// WebSocket). Used to trigger Origin stripping on the outbound
	// request — see onRequestHeaders.
	upgrade bool
}

// readHeaders extracts the X-Sandbox-* values from an Envoy HeaderMap.
// Missing namespace defaults to "default"; missing port defaults to 8888.
// Invalid port (non-numeric) is signaled as port=0 so the caller can
// reject with 400.
func readHeaders(m *corev3.HeaderMap) request {
	r := request{namespace: defaultSandboxNamespace, port: defaultSandboxPort}
	if m == nil {
		return r
	}
	var hasConnectionUpgrade bool
	var hasUpgradeValue bool
	for _, h := range m.Headers {
		switch strings.ToLower(h.Key) {
		case HeaderSandboxID:
			r.id = headerString(h)
		case HeaderSandboxUID:
			r.uid = headerString(h)
		case HeaderSandboxNamespace:
			v := headerString(h)
			if v != "" {
				r.namespace = v
			}
		case HeaderSandboxPort:
			v := headerString(h)
			if v == "" {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				r.port = 0 // signals invalid to caller
				continue
			}
			r.port = n
		case "connection":
			// RFC 7230 §6.7: Connection is a comma-separated list of
			// option tokens. Check case-insensitively for an "upgrade"
			// token, not equality, so values like "keep-alive, upgrade"
			// are recognized.
			for tok := range strings.SplitSeq(headerString(h), ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
					hasConnectionUpgrade = true
					break
				}
			}
		case "upgrade":
			if headerString(h) != "" {
				hasUpgradeValue = true
			}
		}
	}
	// True upgrade requires BOTH headers — matches the predicate
	// httputil.ReverseProxy uses internally.
	r.upgrade = hasConnectionUpgrade && hasUpgradeValue
	return r
}

// headerString returns the header's value as a string, preferring the
// raw_value byte field (modern Envoy) and falling back to the legacy
// string value.
func headerString(h *corev3.HeaderValue) string {
	if len(h.RawValue) > 0 {
		return string(h.RawValue)
	}
	return h.Value
}

// validNamespace mirrors the Python router's namespace check:
//
//	namespace.replace("-", "").isalnum()
//
// At least one ASCII alphanumeric, only ASCII letters/digits/hyphens
// otherwise. Empty input and hyphen-only input both rejected.
func validNamespace(s string) bool {
	if s == "" {
		return false
	}
	hasAlphanum := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			hasAlphanum = true
		case c == '-':
			// allowed
		default:
			return false
		}
	}
	return hasAlphanum
}

// joinHostPort formats "host:port", bracketing IPv6 literals per
// RFC 3986. Sandbox Pods can have IPv6 PodIPs on dual-stack or
// IPv6-only clusters (Pod.Status.PodIP is the primary address, which
// can be v4 or v6); without brackets, Envoy's x-envoy-original-dst-host
// parser would treat the trailing ":port" as part of the address and
// reject the value.
func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// immediate builds an ImmediateResponse ProcessingResponse that ends the
// request at the proxy with the given status code and JSON body. The
// body shape matches the Python router's `{"detail": "..."}` format.
func immediate(status int, body string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode(status)},
				Headers: &extprocv3.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{{
						Header: &corev3.HeaderValue{
							Key:      "content-type",
							RawValue: []byte("application/json"),
						},
						AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
					}},
				},
				Body:    []byte(body),
				Details: fmt.Sprintf("sandbox-router: %d", status),
			},
		},
	}
}
