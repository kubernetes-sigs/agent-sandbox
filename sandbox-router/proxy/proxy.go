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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/net/http/httpguts"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// Handler implements the request-routing core of the sandbox-router. Each
// HTTP request is parsed into a Target and proxied to the upstream sandbox
// with the same body, headers (minus Host), and method.
type Handler struct {
	cfg        *config.Config
	metrics    *observability.Metrics
	propagator propagation.TextMapPropagator
	transport  http.RoundTripper
	cache      Lookup
	authz      authz.Authorizer
	log        logr.Logger
}

// Options bundles the dependencies NewHandler needs. Metrics, Propagator,
// Cache, and Authorizer are optional; nil values produce a router with
// no metrics, a no-op propagator, DNS-only resolution, and AllowAll
// authorization respectively, which is convenient for tests.
type Options struct {
	Config     *config.Config
	Metrics    *observability.Metrics
	Propagator propagation.TextMapPropagator
	// Cache is the Pod-IP lookup used for the KEP-NNNN fast path. When
	// nil, the handler resolves every request via DNS — useful for tests
	// and for deployments running without RBAC for Pod informers.
	Cache Lookup
	// Authorizer guards every proxied request. When nil, the handler
	// uses authz.AllowAll — the Python-compatible default. Set this to
	// a TokenReview authorizer to enforce per-sandbox auth (KEP-NNNN).
	Authorizer authz.Authorizer
	Logger     logr.Logger
}

// NewHandler builds a Handler from o.
func NewHandler(o Options) *Handler {
	if o.Config == nil {
		panic("proxy.NewHandler: Config is required")
	}
	if o.Propagator == nil {
		o.Propagator = propagation.TraceContext{}
	}
	var tr http.RoundTripper = defaultTransport(o.Config)
	// Wrap with retry only if max-retries > 0. The transport is unchanged
	// when retries are disabled so the request path stays a single Dial.
	if o.Config.UpstreamMaxRetries > 0 {
		// Total attempts = 1 (initial) + UpstreamMaxRetries.
		attempts := 1 + o.Config.UpstreamMaxRetries
		var onRetry func(*http.Request, error, int)
		if o.Metrics != nil {
			metrics := o.Metrics
			onRetry = func(req *http.Request, _ error, _ int) {
				metrics.UpstreamRetriesTotal.WithLabelValues(
					observability.SandboxNamespaceFromContext(req.Context()),
				).Inc()
			}
		}
		tr = newRetryTransport(tr, attempts,
			o.Config.UpstreamRetryInitialDelay,
			o.Config.UpstreamRetryMaxDelay,
			onRetry,
		)
	}
	// Wrap with metrics last (outermost) so every upstream byte —
	// including those on the single successful attempt after dial-class
	// retries — is counted exactly once.
	if o.Metrics != nil {
		tr = newMetricsTransport(tr, o.Metrics)
	}
	authorizer := o.Authorizer
	if authorizer == nil {
		authorizer = authz.AllowAll{}
	}
	return &Handler{
		cfg:        o.Config,
		metrics:    o.Metrics,
		propagator: o.Propagator,
		transport:  tr,
		cache:      o.Cache,
		authz:      authorizer,
		log:        o.Logger,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upstreamPath, upstreamRawPath := r.URL.Path, ""
	var pathRouted bool
	target, perr := h.resolveTarget(r, &upstreamPath, &upstreamRawPath, &pathRouted)
	if perr != nil {
		WriteJSONError(w, perr)
		return
	}

	// Make the resolved identity visible to the observability middleware.
	// Tracing and access logging read it back from Labels rather than the
	// request headers directly, because a path-routed request (see
	// resolveTarget) never sets X-Sandbox-* at all — reading headers
	// there would silently show an empty sandbox identity for every
	// browser-facing request.
	if labels := observability.LabelsFromContext(r.Context()); labels != nil {
		labels.SandboxNamespace = target.Namespace
		labels.SandboxID = target.ID
	}

	// Detect Upgrade once, ahead of authorization: the browser-session
	// bootstrap below and the Rewrite callback further down both need
	// it, and computing it in one place keeps them from disagreeing if
	// the predicate ever changes.
	upgrade := isUpgradeRequest(r)

	// Cross-Site WebSocket Hijacking guard. A cookie is an ambient
	// credential — unlike a header or a query parameter, the browser
	// attaches it to a request an attacker's page initiated, and a
	// WebSocket handshake in particular is not subject to the
	// Same-Origin Policy the way a fetch is. This runs BEFORE Authorize
	// (a forged cross-site request should never reach the authorizer,
	// let alone cost a TokenReview API call) and before the Rewrite
	// callback below strips Origin for upgrades. Header- and
	// query-sourced credentials are exempt: a third-party page cannot
	// forge either one.
	credSrc := h.credentialSource(r)
	if credSrc == authz.TokenSourceCookie {
		origin := r.Header.Get("Origin")
		if !isAllowedOrigin(origin, requestOrigin(r, h.cfg.AuthzTrustForwardedProto), h.cfg.AuthzCookieAllowedOrigins) {
			observability.LoggerFromContext(r.Context(), h.log).Info("authorization denied: origin not allowed for cookie-authenticated request",
				"sandbox", target.ID,
				"namespace", target.Namespace,
				"origin", origin,
			)
			if h.metrics != nil {
				h.metrics.AuthzDecisionsTotal.WithLabelValues(target.Namespace, "deny").Inc()
			}
			WriteJSONError(w, &Error{Status: http.StatusForbidden, Detail: "origin not allowed for cookie-authenticated request"})
			return
		}
	}

	// Authorization. Implementations are expected to pull whatever
	// credential they need (TLS cert, Bearer token, custom header) off
	// the request and either allow or return one of the sentinel
	// errors in package authz. The default AllowAll authorizer wired in
	// by NewHandler always permits, preserving the Python router's
	// no-auth contract.
	if err := h.authz.Authorize(r.Context(), r, target.Namespace, target.ID); err != nil {
		status := authz.HTTPStatusFor(err)
		observability.LoggerFromContext(r.Context(), h.log).Info("authorization denied",
			"sandbox", target.ID,
			"namespace", target.Namespace,
			"status", status,
			"error", err.Error(),
		)
		if h.metrics != nil {
			h.metrics.AuthzDecisionsTotal.WithLabelValues(target.Namespace, "deny").Inc()
		}
		WriteJSONError(w, &Error{Status: status, Detail: err.Error()})
		return
	}
	if h.metrics != nil {
		h.metrics.AuthzDecisionsTotal.WithLabelValues(target.Namespace, "allow").Inc()
	}

	// Browser-session bootstrap: this request just proved (via the
	// Authorize call above) that it holds a valid credential presented
	// through the query parameter. Set it as a cookie scoped to this
	// sandbox and redirect, so every later request — including a
	// WebSocket handshake, which cannot carry a header and, in any real
	// browser flow, never carries this query parameter either — relies
	// on the cookie a browser attaches automatically.
	if h.maybeBootstrapCookie(w, r, target, credSrc, upgrade, pathRouted) {
		return
	}

	target0 := target // capture for closures
	outboundRawQuery := r.URL.RawQuery
	if h.cfg.AuthzCookieQueryParam != "" {
		// Never forward the bootstrap credential to the sandbox itself.
		// In practice this only fires for the non-GET/HEAD and upgrade
		// edge cases maybeBootstrapCookie declines to redirect — the
		// normal flow already returned above.
		outboundRawQuery = stripQueryParam(outboundRawQuery, h.cfg.AuthzCookieQueryParam)
	}
	// Resolve once per request so the ErrorHandler can see which path
	// produced the IP (cache vs DNS vs override) and invalidate the cache
	// entry on dial-class failures. The Rewrite callback re-uses the URL.
	upstreamURL, src := target0.Resolve("http", h.cfg.ClusterDomain, upstreamPath, outboundRawQuery, h.cache)
	if upstreamRawPath != "" {
		// Only ever set for a path-routed request (see resolveTarget).
		// Target.Resolve only assigns URL.Path, so without this a path-
		// routed request carrying an encoded separator in its upstream
		// portion (e.g. a filename containing "/", sent as "%2F") would
		// reach the sandbox already decoded, silently renaming the
		// resource it addresses. net/url only honors RawPath when it is
		// a valid encoding of Path (see url.URL.EscapedPath's doc) —
		// resolveTarget/ParsePathRoute guarantee that pairing holds.
		upstreamURL.RawPath = upstreamRawPath
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = upstreamURL
			// Clear inbound Host so net/http picks the URL host. Matches the
			// Python router's behavior of stripping Host before forwarding.
			pr.Out.Host = ""
			// Strip Authorization before forwarding. The router consumes
			// it (e.g. --authz-mode=tokenreview validates a Bearer token
			// via the K8s TokenReview API); the sandbox must not see the
			// caller's credential because it would let any sandbox
			// impersonate the caller against the K8s API or any other
			// Bearer-protected service. Matches the Python router, which
			// strips Authorization right next to Host.
			pr.Out.Header.Del("Authorization")
			// Likewise, never forward our own session cookie — it is as
			// much a credential as the Authorization header above. Any
			// other cookie the client sent (e.g. one of code-server's
			// own) passes through untouched.
			//
			// Values(), not Get(): a client is free to send "Cookie" as
			// more than one header line (rare from a real browser, which
			// always combines its cookies into one, but not disallowed),
			// and Get only ever returns the first. Joining every value
			// with "; " before stripping and writing back a single
			// header means the credential is found and removed
			// regardless of which line it arrived on, and nothing else
			// the client sent is silently dropped along with it.
			if h.cfg.AuthzCookieName != "" {
				if vs := pr.Out.Header.Values("Cookie"); len(vs) > 0 {
					if stripped := stripCookieFromHeader(strings.Join(vs, "; "), h.cfg.AuthzCookieName); stripped == "" {
						pr.Out.Header.Del("Cookie")
					} else {
						pr.Out.Header.Set("Cookie", stripped)
					}
				}
			}
			// SetXForwarded uses Set() for Host + Proto (overwrites,
			// safe) but APPENDS to any existing X-Forwarded-For — so a
			// client-supplied "X-Forwarded-For: 1.2.3.4" would land
			// at the upstream as "1.2.3.4, <real_client_ip>", letting
			// the client poison the chain to claim a different source.
			// Strip the inbound value first so SetXForwarded writes a
			// single trusted entry: the actual client IP the router
			// observed. If a trusted upstream proxy needs to preserve
			// its own X-Forwarded-For, that trust should be wired
			// explicitly rather than blanket-trusting whatever the
			// inbound connection carries.
			pr.Out.Header.Del("X-Forwarded-For")
			// X-Forwarded-{For,Host,Proto} so the upstream sandbox can
			// reconstruct the client-visible URL for self-links and
			// redirects. SetXForwarded is the canonical helper —
			// always present, no allocation when the headers already
			// exist, and respects the inbound request's TLS state.
			pr.SetXForwarded()
			// Origin is a hard problem on upgrade. Many WebSocket
			// backends (vscode-server is the classic case) validate
			// Origin == Host for CSRF protection. We rewrite Host to
			// the upstream's address, so a client-supplied Origin
			// pointing at the router's external hostname will mismatch
			// and the backend rejects the upgrade with a 1006 close.
			// Dropping Origin tells the backend "no Origin assertion
			// available", which CSRF-aware backends typically allow
			// for non-browser callers; backends that require a present
			// Origin still need a separate fix on their end, but the
			// vscode/Jupyter family work as-is. We only strip on
			// upgrade so normal HTTP CORS preflights are unaffected.
			if upgrade {
				pr.Out.Header.Del("Origin")
			}
			// Inject trace context into the outbound request so the sandbox
			// sees a continuation of the inbound trace.
			h.propagator.Inject(pr.Out.Context(), propagation.HeaderCarrier(pr.Out.Header))
		},
		Transport:     h.transport,
		FlushInterval: -1, // immediate flush for SSE / streaming responses
		ErrorHandler: func(w http.ResponseWriter, errReq *http.Request, err error) {
			h.recordUpstreamErrorReason(target0.Namespace, classifyError(err))
			// KEP-NNNN: actively invalidate the cache entry when a dial
			// failure indicates the IP itself is dead (timeout, no
			// route), so the next request falls through to DNS instead
			// of retrying the same stale IP. isDeadHostDialError rather
			// than classifyError's string label because dial-time
			// timeouts are labeled "timeout" yet the IP is just as dead;
			// and NOT plain isRetriableDialError because a connection
			// refusal proves a live host — evicting on it would let a
			// caller-side mistake (wrong X-Sandbox-Port) strand the only
			// working route for a warm-pool sandbox until resync. We only
			// invalidate when the IP we tried actually came from the
			// cache — a DNS or PodIP-header failure means the cache had
			// nothing useful to evict — and evict by the same key the
			// entry was resolved with: UID for the fast path,
			// namespace/name (plus the failed IP, so a recreated Pod's
			// fresh entry survives) for the name index.
			if h.cache != nil && isDeadHostDialError(err) {
				invalidated := false
				switch {
				case src == SourceCache && target0.UID != "":
					invalidated = h.cache.Invalidate(types.UID(target0.UID))
				case src == SourceCacheName:
					// SplitHostPort strips the port and the brackets
					// JoinHostPort added around IPv6 literals.
					if ip, _, sperr := net.SplitHostPort(upstreamURL.Host); sperr == nil {
						invalidated = h.cache.InvalidateByName(target0.Namespace, target0.ID, ip)
					}
				}
				if invalidated && h.metrics != nil {
					h.metrics.CacheInvalidationsTotal.WithLabelValues(target0.Namespace).Inc()
				}
			}
			// Use the per-request logger from context so the trace ID is
			// included alongside the upstream failure detail.
			observability.LoggerFromContext(errReq.Context(), h.log).Error(err,
				"upstream connect failure",
				"sandbox", target0.ID,
				"namespace", target0.Namespace,
				"source", string(src),
			)
			WriteJSONError(w, &Error{
				Status: http.StatusBadGateway,
				Detail: fmt.Sprintf("Could not connect to the backend sandbox: %s", target0.ID),
			})
		},
	}

	// Bound the upstream request lifetime by the configured proxy timeout,
	// EXCEPT for protocol upgrades (WebSockets, raw TCP via CONNECT). An
	// upgraded connection is long-lived by design — e.g. code-server holds
	// a single WebSocket open for the whole editing session — and applying
	// ProxyTimeout to it would tear down a healthy session at the timeout
	// boundary (180s default → client sees WebSocket close 1006). Once the
	// 101 handshake is done the connection's TCP keepalive is the
	// liveness signal, not our handler context.
	ctx := r.Context()
	if !upgrade {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.cfg.ProxyTimeout)
		defer cancel()
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}

// resolveTarget parses the routing Target for r, preferring the path-based
// form when --path-routing-prefix is configured and r.URL.EscapedPath()
// matches it, and falling through to X-Sandbox-* headers otherwise —
// byte-identical to the header parsing this replaced when the prefix is
// unset (the default), so header-only deployments are unaffected.
//
// On a path match, *upstreamPath and *upstreamRawPath are rewritten to the
// portion of the path the upstream sandbox should actually see
// (prefix/namespace/id/port stripped) in, respectively, decoded and
// originally-escaped form — both are needed together to preserve an
// encoded separator (e.g. "%2F") through to the outbound request, see
// ParsePathRoute. Both are left untouched for header-routed requests,
// which carry no routing prefix to strip; *upstreamRawPath in particular
// stays "" there, exactly matching pre-path-routing behavior of never
// setting Resolve's outbound URL.RawPath at all.
//
// *pathRouted reports which branch actually resolved the target — the
// browser-session bootstrap (maybeBootstrapCookie) must only fire for a
// path-routed request, since the cookie it would set is scoped to a
// Path shaped like the routing prefix and can never match a
// header-routed request's URL.
func (h *Handler) resolveTarget(r *http.Request, upstreamPath, upstreamRawPath *string, pathRouted *bool) (Target, *Error) {
	if h.cfg.PathRoutingPrefix != "" {
		if route, matched, perr := ParsePathRoute(h.cfg.PathRoutingPrefix, r.URL.EscapedPath()); matched {
			if perr != nil {
				return Target{}, perr
			}
			*upstreamPath = route.UpstreamPath
			*upstreamRawPath = route.UpstreamRawPath
			*pathRouted = true
			return route.Target, nil
		}
	}
	return ParseSandboxHeaders(r.Header, ParseOptions{AllowLoopbackPodIP: h.cfg.AllowLoopbackPodIP})
}

// isUpgradeRequest reports whether r is asking the server to switch
// protocols (RFC 7230 §6.7). The check follows the same rule
// httputil.ReverseProxy uses internally to recognize upgrade requests:
// a Connection header with a "Upgrade" token AND a non-empty Upgrade
// header. WebSockets are the common case; SPDY / HTTP/2 cleartext
// upgrade also qualify.
func isUpgradeRequest(r *http.Request) bool {
	if !httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") {
		return false
	}
	return r.Header.Get("Upgrade") != ""
}

// defaultTransport builds the shared *http.Transport used for upstream
// requests. Values mirror Go's DefaultTransport (minus Proxy — see
// below) plus a configurable ResponseHeaderTimeout and disabled HTTP/2
// to backends (sandboxes are h1 today; opting in to h2 to backends
// would require negotiation we don't want to introduce silently).
//
// Note Proxy is deliberately unset (nil), NOT http.ProxyFromEnvironment.
// The router only dials cluster-internal addresses: Pod IPs from the
// cache / X-Sandbox-Pod-IP header, or "<id>.<ns>.svc.<cluster-domain>"
// DNS names. If a deployment has HTTP_PROXY / HTTPS_PROXY set in the
// router pod's env without a matching NO_PROXY covering the cluster's
// Pod CIDR and DNS suffix, ProxyFromEnvironment would route those
// internal dials through an external proxy — connectivity breaks and
// in the worst case Pod-IP traffic leaves the cluster. Defaulting to
// no proxy makes "router goes direct to the sandbox" the guarantee.
func defaultTransport(cfg *config.Config) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
	}
}

// recordUpstreamErrorReason bumps the upstream-error counter with a
// pre-classified reason label.
func (h *Handler) recordUpstreamErrorReason(namespace, reason string) {
	if h.metrics == nil {
		return
	}
	h.metrics.UpstreamErrorsTotal.WithLabelValues(namespace, reason).Inc()
}

// classifyError turns an arbitrary RoundTrip error into a low-cardinality
// label value so the upstream_errors_total counter does not explode.
func classifyError(err error) string {
	if err == nil {
		return "unknown"
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case strings.Contains(err.Error(), "tls"):
		return "tls"
	case strings.Contains(err.Error(), "EOF"):
		return "eof"
	case strings.Contains(err.Error(), "connection refused"),
		strings.Contains(err.Error(), "no such host"),
		strings.Contains(err.Error(), "dial"):
		return "dial"
	}
	return "other"
}

// metricsTransport wraps an http.RoundTripper and increments Prometheus
// counters for every byte flowing to / from the upstream sandbox. The
// sandbox_namespace label is read from the per-request *Labels the proxy
// handler attached to the request context earlier in ServeHTTP — same
// mechanism the rest of the observability stack uses.
//
// The wrapping is applied outside retryTransport so retries that fail at
// dial time (before any body bytes move) don't double-count; retries on
// dial failures are safe here because http.Transport only consumes the
// body after a successful connect.
type metricsTransport struct {
	base    http.RoundTripper
	metrics *observability.Metrics
}

func newMetricsTransport(base http.RoundTripper, metrics *observability.Metrics) *metricsTransport {
	return &metricsTransport{base: base, metrics: metrics}
}

// RoundTrip implements http.RoundTripper.
func (t *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	namespace := observability.SandboxNamespaceFromContext(req.Context())

	// Wrap the request body so bytes streamed to the upstream are
	// counted toward upstream_tx_bytes_total. The counter is bound to
	// the resolved namespace up front; WithLabelValues is a map lookup
	// but the series is reused for the lifetime of the process so the
	// per-call cost is trivial.
	if req.Body != nil {
		req.Body = &upstreamCountingReadCloser{
			rc:      req.Body,
			counter: t.metrics.UpstreamTxBytesTotal.WithLabelValues(namespace),
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Wrap the response body so bytes streamed back from the upstream
	// are counted toward upstream_rx_bytes_total. httputil.ReverseProxy
	// copies resp.Body to the client via io.Copy and then calls Close,
	// so the counter is incremented lazily as bytes flow — SSE /
	// streaming responses are accounted for correctly because the
	// counting happens per Read, not per response.
	if resp.Body != nil {
		resp.Body = &upstreamCountingReadCloser{
			rc:      resp.Body,
			counter: t.metrics.UpstreamRxBytesTotal.WithLabelValues(namespace),
		}
	}

	return resp, nil
}

// upstreamCountingReadCloser wraps an io.ReadCloser, counts bytes read,
// and increments a Prometheus counter in real time. Incrementing per Read
// (rather than accumulating locally and flushing at Close) keeps the
// counters accurate even if a streaming response is abandoned mid-flight
// — the bytes that did flow are still attributed.
type upstreamCountingReadCloser struct {
	rc      io.ReadCloser
	counter prometheus.Counter
}

func (c *upstreamCountingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.counter.Add(float64(n))
	}
	return n, err
}

func (c *upstreamCountingReadCloser) Close() error {
	return c.rc.Close()
}
