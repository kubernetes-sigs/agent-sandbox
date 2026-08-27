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

// Package config defines the runtime configuration for the sandbox-router
// binary.
//
// Resolution order: ConfigMap value > CLI flag > config file > env > built-in default.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// MTLSMode controls how the router validates client certificates on incoming
// TLS connections.
type MTLSMode string

const (
	// MTLSOff disables client certificate verification.
	MTLSOff MTLSMode = "off"
	// MTLSOptional validates a client certificate if presented, otherwise
	// allows the connection.
	MTLSOptional MTLSMode = "optional"
	// MTLSRequired rejects any connection that does not present a valid client
	// certificate issued by a trusted CA.
	MTLSRequired MTLSMode = "required"
)

// CookieSameSite selects the SameSite attribute of the browser-session
// cookie (see Config.AuthzCookieName).
type CookieSameSite string

const (
	// CookieSameSiteLax covers same-site embedding: a page and the
	// router sharing a registrable domain (e.g. a page on
	// atenea.example.com embedding an iframe served from
	// sandboxes.example.com — same site, different origin) get the
	// cookie on the iframe load and any WebSocket handshake it opens,
	// without requiring HTTPS. This is the default because it is both
	// the common deployment shape and the safer one.
	CookieSameSiteLax CookieSameSite = "lax"
	// CookieSameSiteStrict never sends the cookie on a cross-site
	// request, including the top-level navigation that would otherwise
	// bootstrap it — too strict for the "open in a new tab" case this
	// feature exists for.
	CookieSameSiteStrict CookieSameSite = "strict"
	// CookieSameSiteNone is for genuinely cross-site embedding
	// (integrator and router on different registrable domains).
	// Browsers require Secure on a SameSite=None cookie and refuse to
	// set it otherwise; some additionally block it outright as a
	// third-party cookie, which no router-side setting can work around.
	CookieSameSiteNone CookieSameSite = "none"
)

// AuthzMode selects the per-request authorization strategy.
type AuthzMode string

const (
	// AuthzAllowAll permits every request (Python router default).
	AuthzAllowAll AuthzMode = "allow-all"
	// AuthzTokenReview authenticates each request via the K8s
	// TokenReview API. Per-sandbox authorization beyond authentication
	// is out of v1 scope.
	AuthzTokenReview AuthzMode = "tokenreview"
	// AuthzScopedToken authorizes each request against a signed,
	// per-sandbox token instead of a cluster-verifiable K8s
	// credential: the token is bound to one (namespace, name) pair at
	// mint time (see authz.MintScopedToken) and the router rejects it
	// with 403 against any other sandbox. Use this when the caller
	// should never need to hold a K8s Bearer token at all.
	AuthzScopedToken AuthzMode = "scoped-token"
)

// Config is the parsed runtime configuration. All fields are populated by
// RegisterFlags + flag.Parse and validated by Validate.
type Config struct {
	// HTTPAddr is the address for the plain-HTTP proxy listener. Empty disables
	// plain HTTP.
	HTTPAddr string
	// HTTPSAddr is the address for the TLS proxy listener. Empty disables TLS.
	HTTPSAddr string
	// MetricsAddr is the address for the Prometheus /metrics endpoint.
	MetricsAddr string
	// ProbeAddr is the address for the /healthz and /readyz endpoints.
	ProbeAddr string

	// TLSCertFile is the path to the PEM-encoded server certificate.
	TLSCertFile string
	// TLSKeyFile is the path to the PEM-encoded server private key.
	TLSKeyFile string
	// TLSClientCAFile is the path to the PEM-encoded CA bundle used to verify
	// client certificates when MTLSMode is optional or required.
	TLSClientCAFile string
	// MTLSMode selects the client-certificate verification policy.
	MTLSMode MTLSMode

	// ClusterDomain is the Kubernetes cluster DNS suffix used to build target
	// service FQDNs (e.g. "cluster.local"). Honors CLUSTER_DOMAIN.
	ClusterDomain string
	// ProxyTimeout bounds the total time spent proxying a single request to
	// an upstream sandbox. Honors PROXY_TIMEOUT_SECONDS (numeric seconds).
	ProxyTimeout time.Duration
	// ResponseHeaderTimeout bounds the time spent waiting for the upstream
	// to start sending the response headers.
	ResponseHeaderTimeout time.Duration
	// ShutdownTimeout bounds the time each HTTP server is allowed to drain
	// in-flight requests on SIGTERM.
	ShutdownTimeout time.Duration
	// UpstreamMaxRetries is the number of additional attempts the router
	// will make on dial-class failures. 0 disables retries entirely; the
	// default smooths the case where a freshly-created sandbox's DNS or
	// pod listener isn't ready yet. Only dial-time failures are retried —
	// errors that surface after the request body may have been sent
	// (response timeouts, mid-stream EOF) are returned as-is.
	UpstreamMaxRetries int
	// UpstreamRetryInitialDelay is the wait before the first retry.
	// Subsequent waits double up to UpstreamRetryMaxDelay.
	UpstreamRetryInitialDelay time.Duration
	// UpstreamRetryMaxDelay caps the per-iteration backoff.
	UpstreamRetryMaxDelay time.Duration
	// MaxRequestBodyBytes optionally caps the inbound request body size.
	// 0 means unlimited.
	MaxRequestBodyBytes int64

	// AllowLoopbackPodIP, when true, lets X-Sandbox-Pod-IP carry a
	// loopback address (127.0.0.0/8 or ::1). The default-false
	// behavior matches the Python router: loopback/link-local/
	// multicast/unspecified addresses are rejected with 400 so the
	// router can't be turned into an SSRF gadget pointed at the
	// router pod's own loopback or cloud metadata endpoints.
	//
	// Enable only when the sandbox runs as a sidecar in the same Pod
	// as the router (so 127.0.0.1 is the correct dial address) or in
	// integration tests that spin up an httptest backend on
	// localhost. Link-local, multicast, and unspecified addresses
	// stay rejected even when this flag is on.
	AllowLoopbackPodIP bool

	// PathRoutingPrefix, when non-empty, additionally lets a caller address
	// a sandbox via a URL path segment instead of X-Sandbox-* headers:
	// <PathRoutingPrefix>/<namespace>/<id>/<port>/<rest...>. This is the
	// only way a browser can reach a WebSocket-dependent backend inside a
	// sandbox (a web IDE's terminal, a dev server's HMR socket) through
	// this router: a page or an iframe cannot set custom request headers
	// at all, and the WebSocket handshake specifically has no API for
	// them either (see the "Browser-facing traffic" section of the
	// package README for why — it is a platform limitation, not
	// something a client-side workaround like a Service Worker can paper
	// over, since WebSocket handshakes are not exposed to the fetch
	// event). A page loaded under this prefix carries the same prefix
	// into any relative WebSocket URL it opens, so the routing identity
	// travels with zero client-side code.
	//
	// Off by default (""), so header-only behavior is completely
	// unchanged unless an operator opts in. When set, a request path
	// starting with this prefix is parsed for routing information
	// FIRST; a path that does not match falls straight through to
	// X-Sandbox-* header parsing, unmodified.
	//
	// X-Sandbox-Pod-IP and X-Sandbox-UID have no path equivalent, by
	// design: both are trust-sensitive dial-target overrides meant for
	// SDKs that already hold cluster-internal knowledge, never for a
	// browser tab.
	PathRoutingPrefix string

	// EnableTracing enables OTel tracing via the OTLP gRPC exporter. The
	// exporter endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT.
	EnableTracing bool
	// EnableOTelMetrics enables periodic OTLP gRPC push of every series in
	// the Prometheus registry. The /metrics endpoint stays active either
	// way; this is additive. Endpoint comes from
	// OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT.
	EnableOTelMetrics bool
	// AccessLog enables one structured log line per inbound request on the
	// proxy port. Health and metrics endpoints are not logged.
	AccessLog bool

	// PrintVersion makes the binary print version info and exit.
	PrintVersion bool

	// ConfigFile is the path of a YAML config file applied during startup.
	// Set via --config or SANDBOX_ROUTER_CONFIG. Stored for introspection;
	// the actual file load happens in main() before flag.Parse.
	ConfigFile string

	// ConfigMapName is the name of a Kubernetes ConfigMap whose data keys
	// are applied as configuration (same key=value semantics as the YAML
	// config file). Defaults to "sandbox-router-config". Set via
	// --config-configmap-name or SANDBOX_ROUTER_CONFIG_CONFIGMAP_NAME.
	// The ConfigMap is read from the namespace the router Pod runs in
	// (detected from the in-Pod service-account namespace file).
	ConfigMapName string

	// CacheEnabled turns on the in-process Pod-IP cache. When true the
	// router builds an informer for sandbox-owned Pods and serves the
	// KEP-NNNN fast path: requests carrying X-Sandbox-UID are dialed at
	// the live PodIP, bypassing DNS. When false (the default) the router
	// behaves like the Python original — DNS only.
	CacheEnabled bool
	// CacheNamespace optionally narrows the Pod informer to a single
	// namespace. Empty means cluster-wide (recommended; sandboxes can
	// live in many namespaces).
	CacheNamespace string
	// Kubeconfig is the path to a kubeconfig file used to build the
	// informer client. Empty means use in-cluster config. Honors the
	// standard KUBECONFIG env var.
	Kubeconfig string

	// AuthzMode selects how every inbound request is authorized.
	// Defaults to allow-all (Python compatibility); set to tokenreview
	// to enforce Bearer-token authentication via the K8s TokenReview
	// API.
	AuthzMode AuthzMode
	// AuthzTokenReviewTTL bounds how long a TokenReview decision is
	// cached. Shorter values catch revocations sooner; longer values
	// reduce apiserver load.
	AuthzTokenReviewTTL time.Duration
	// AuthzTokenReviewCacheSize is the maximum number of cached
	// TokenReview decisions before LRU eviction starts.
	AuthzTokenReviewCacheSize int
	// AuthzTokenReviewRequireToken, when true, rejects requests that
	// arrive without an Authorization: Bearer ... header. When false,
	// tokenless requests are allowed through — useful during rollouts.
	AuthzTokenReviewRequireToken bool
	// AuthzTokenReviewAudiences, when non-empty, asks the apiserver to
	// verify that the token was minted for one of these audiences.
	// Projected ServiceAccount tokens carry an aud claim that must
	// match. Empty disables the audience check.
	AuthzTokenReviewAudiences []string

	// AuthzScopedTokenSecretFile is the path to a file holding the
	// shared HMAC-SHA256 secret used to verify scoped tokens (see
	// authz.ScopedTokenAuthorizer). Required when AuthzMode is
	// scoped-token. The router never generates this secret — whoever
	// mints tokens (typically the Sandbox controller) and the router
	// must share it out-of-band, e.g. the same K8s Secret mounted into
	// both.
	AuthzScopedTokenSecretFile string

	// AuthzCookieName, when non-empty, additionally lets the configured
	// Authorizer accept a credential carried in a cookie of this name —
	// the only credential channel a browser attaches automatically to
	// every request toward the same origin, including a WebSocket
	// handshake (see authz.TokenSourceCookie and the "Browser-facing
	// traffic" section of the README for why a header or a bare query
	// parameter cannot do this job on their own). Requires
	// PathRoutingPrefix: without a browser-reachable route there is
	// nothing for this cookie to protect, and its Path attribute (see
	// AuthzCookieQueryParam) is derived from the prefix plus the
	// sandbox being addressed. Meaningless with AuthzMode ==
	// AuthzAllowAll, which authorizes everything regardless.
	AuthzCookieName string
	// AuthzCookieQueryParam, when non-empty, is the name of a URL query
	// parameter that bootstraps the cookie above. A GET or HEAD,
	// non-upgrade request presenting a valid credential here gets
	// AuthzCookieName set — scoped by Path to exactly the
	// (namespace, id, port) it authorized — and is redirected (302) to
	// the same URL with the parameter stripped, so the credential
	// spends the smallest possible time exposed in the URL, browser
	// history, and any Referer a subsequent page load might send.
	// Requires AuthzCookieName.
	AuthzCookieQueryParam string
	// AuthzCookieSameSite sets the SameSite attribute of the
	// bootstrapped cookie. Defaults to "lax".
	AuthzCookieSameSite CookieSameSite
	// AuthzCookieInsecure omits the Secure attribute from the
	// bootstrapped cookie. Only for local development over plain HTTP;
	// incompatible with AuthzCookieSameSite == CookieSameSiteNone,
	// which browsers refuse to accept without Secure.
	AuthzCookieInsecure bool
	// AuthzCookieAllowedOrigins is the allowlist checked against the
	// Origin header whenever a request's credential came from the
	// cookie (never for header- or query-sourced credentials, which a
	// third-party page cannot forge). A same-origin request — Origin's
	// host equals the request's Host — is always allowed regardless of
	// this list; a request with no Origin header at all is also let
	// through here, since it carries nothing for this check to inspect
	// (browsers reliably send Origin on exactly the requests this list
	// exists to gate: cross-site requests and every WebSocket
	// handshake). This check exists because a cookie is an ambient
	// credential any origin's page can ride on — a WebSocket handshake
	// in particular is not subject to the Same-Origin Policy the way a
	// fetch is — and the router cannot lean on the upstream sandbox to
	// reject a bad Origin itself, since it strips Origin on upgrades
	// for code-server's benefit (see the "WebSockets and other protocol
	// upgrades" section of the README). Each entry has the form
	// scheme://host[:port].
	AuthzCookieAllowedOrigins []string
	// AuthzTrustForwardedProto makes requestOrigin() (the same-origin
	// half of the cookie-authz check above) read the scheme from the
	// first value of X-Forwarded-Proto instead of r.TLS != nil, when
	// that header is present. Off by default: r.TLS only reflects
	// whether TLS terminated in this process, which is false for every
	// request behind a TLS-terminating load balancer or Gateway — the
	// common production shape — so without this flag every same-origin
	// request behind one is misclassified as cross-origin and falls
	// through to AuthzCookieAllowedOrigins (or gets rejected if that
	// list doesn't happen to name the deployment's own public origin).
	// Only turn this on when the router is reachable exclusively
	// through a proxy that sets this header itself and strips any
	// client-supplied one — otherwise a client with direct network
	// access to the router could forge its own scheme and defeat the
	// same-origin check this exists to make actually match reality.
	// Has no effect without AuthzCookieName.
	AuthzTrustForwardedProto bool
}

// Defaults returns a Config populated with the default values used when no
// flag overrides are present.
func Defaults() Config {
	return Config{
		HTTPAddr:                  ":8080",
		HTTPSAddr:                 "",
		MetricsAddr:               ":9090",
		ProbeAddr:                 ":8081",
		MTLSMode:                  MTLSOff,
		ClusterDomain:             "cluster.local",
		ProxyTimeout:              180 * time.Second,
		ResponseHeaderTimeout:     30 * time.Second,
		ShutdownTimeout:           30 * time.Second,
		UpstreamMaxRetries:        3,
		UpstreamRetryInitialDelay: 200 * time.Millisecond,
		UpstreamRetryMaxDelay:     800 * time.Millisecond,
		AccessLog:                 true,
		AuthzMode:                 AuthzAllowAll,
		AuthzTokenReviewTTL:       30 * time.Second,
		AuthzTokenReviewCacheSize: 2048,
		AuthzCookieSameSite:       CookieSameSiteLax,
		ConfigMapName:             "sandbox-router-config",
	}
}

// Validate checks that the resolved configuration is internally consistent.
// It returns the first error encountered.
func (c *Config) Validate() error {
	if c.HTTPAddr == "" && c.HTTPSAddr == "" {
		return errors.New("at least one of --http-bind-address or --https-bind-address must be set")
	}

	switch c.MTLSMode {
	case MTLSOff, MTLSOptional, MTLSRequired:
	default:
		return fmt.Errorf("invalid --mtls-mode %q (want off, optional, or required)", c.MTLSMode)
	}

	if c.HTTPSAddr != "" {
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return errors.New("--tls-cert-file and --tls-key-file are required when --https-bind-address is set")
		}
	}

	if c.MTLSMode != MTLSOff {
		if c.HTTPSAddr == "" {
			return fmt.Errorf("--mtls-mode=%s requires --https-bind-address to be set", c.MTLSMode)
		}
		if c.TLSClientCAFile == "" {
			return fmt.Errorf("--mtls-mode=%s requires --tls-client-ca-file", c.MTLSMode)
		}
	}

	if c.ProxyTimeout <= 0 {
		return fmt.Errorf("--proxy-timeout must be positive, got %s", c.ProxyTimeout)
	}
	if c.ResponseHeaderTimeout <= 0 {
		return fmt.Errorf("--response-header-timeout must be positive, got %s", c.ResponseHeaderTimeout)
	}
	if c.ShutdownTimeout < 0 {
		return fmt.Errorf("--shutdown-timeout must be non-negative, got %s", c.ShutdownTimeout)
	}
	if c.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("--max-request-body-bytes must be non-negative, got %d", c.MaxRequestBodyBytes)
	}
	if c.ClusterDomain == "" {
		return errors.New("--cluster-domain must not be empty")
	}
	if c.PathRoutingPrefix != "" {
		if !strings.HasPrefix(c.PathRoutingPrefix, "/") {
			return fmt.Errorf("--path-routing-prefix must start with \"/\", got %q", c.PathRoutingPrefix)
		}
		if strings.HasSuffix(c.PathRoutingPrefix, "/") {
			return fmt.Errorf("--path-routing-prefix must not end with \"/\", got %q", c.PathRoutingPrefix)
		}
		// A prefix containing whitespace or control characters is
		// almost certainly a copy-paste or shell-quoting mistake (a
		// trailing newline from a config template, a stray tab) rather
		// than an intentional value — nothing legitimate needs one here,
		// and letting it through would make ParsePathRoute's prefix match
		// silently fail against every real request while giving no hint
		// why. Reject it at startup, where it is loud, instead of at
		// request time, where it isn't.
		if strings.ContainsFunc(c.PathRoutingPrefix, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			return fmt.Errorf("--path-routing-prefix must not contain whitespace or control characters, got %q", c.PathRoutingPrefix)
		}
	}
	if c.UpstreamMaxRetries < 0 {
		return fmt.Errorf("--upstream-max-retries must be non-negative, got %d", c.UpstreamMaxRetries)
	}
	if c.UpstreamRetryInitialDelay < 0 {
		return fmt.Errorf("--upstream-retry-initial-delay must be non-negative, got %s", c.UpstreamRetryInitialDelay)
	}
	if c.UpstreamRetryMaxDelay < 0 {
		return fmt.Errorf("--upstream-retry-max-delay must be non-negative, got %s", c.UpstreamRetryMaxDelay)
	}

	switch c.AuthzMode {
	case AuthzAllowAll, AuthzTokenReview, AuthzScopedToken:
	default:
		return fmt.Errorf("invalid --authz-mode %q (want allow-all, tokenreview, or scoped-token)", c.AuthzMode)
	}
	if c.AuthzTokenReviewTTL <= 0 {
		return fmt.Errorf("--authz-tokenreview-ttl must be positive, got %s", c.AuthzTokenReviewTTL)
	}
	if c.AuthzTokenReviewCacheSize <= 0 {
		return fmt.Errorf("--authz-tokenreview-cache-size must be positive, got %d", c.AuthzTokenReviewCacheSize)
	}
	if c.AuthzMode == AuthzScopedToken && c.AuthzScopedTokenSecretFile == "" {
		return errors.New("--authz-scoped-token-secret-file is required when --authz-mode=scoped-token")
	}

	switch c.AuthzCookieSameSite {
	case CookieSameSiteLax, CookieSameSiteStrict, CookieSameSiteNone:
	default:
		return fmt.Errorf("invalid --authz-cookie-samesite %q (want lax, strict, or none)", c.AuthzCookieSameSite)
	}
	if c.AuthzCookieQueryParam != "" && c.AuthzCookieName == "" {
		return errors.New("--authz-cookie-query-param requires --authz-cookie-name")
	}
	if c.AuthzCookieName != "" {
		if c.PathRoutingPrefix == "" {
			return errors.New("--authz-cookie-name requires --path-routing-prefix: without it there is no browser-reachable route for the cookie to protect")
		}
		if c.AuthzMode == AuthzAllowAll {
			return errors.New("--authz-cookie-name has no effect with --authz-mode=allow-all, which authorizes every request regardless")
		}
		if !validTokenName(c.AuthzCookieName) {
			return fmt.Errorf("--authz-cookie-name %q must be a valid cookie-name token (letters, digits, \"-\", \"_\", \".\", \"~\")", c.AuthzCookieName)
		}
	}
	if c.AuthzCookieQueryParam != "" && !validTokenName(c.AuthzCookieQueryParam) {
		return fmt.Errorf("--authz-cookie-query-param %q must be a valid token (letters, digits, \"-\", \"_\", \".\", \"~\")", c.AuthzCookieQueryParam)
	}
	if c.AuthzCookieSameSite == CookieSameSiteNone && c.AuthzCookieInsecure {
		return errors.New("--authz-cookie-samesite=none requires the cookie to be Secure; it is incompatible with --authz-cookie-insecure")
	}
	for _, origin := range c.AuthzCookieAllowedOrigins {
		if !validOriginPattern(origin) {
			return fmt.Errorf("--authz-cookie-allowed-origins entry %q must have the form scheme://host[:port], with no path/query/fragment", origin)
		}
	}
	if len(c.AuthzCookieAllowedOrigins) > 0 && c.AuthzCookieName == "" {
		return errors.New("--authz-cookie-allowed-origins has no effect without --authz-cookie-name, which is what runs the same-origin check it configures")
	}
	if c.AuthzTrustForwardedProto && c.AuthzCookieName == "" {
		return errors.New("--authz-trust-forwarded-proto has no effect without --authz-cookie-name, which is what runs the same-origin check it affects")
	}
	return nil
}

// validTokenName reports whether s is a non-empty string made only of
// characters that are safe, unambiguous, and unescaped in every context
// this package uses a name in: an RFC 6265 cookie-name, a URL query
// parameter key, and a Go flag value copied verbatim into a Set-Cookie
// header. Deliberately conservative — there is no legitimate reason for
// an operator-chosen name to need anything outside this set.
func validTokenName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '~':
		default:
			return false
		}
	}
	return true
}

// validOriginPattern reports whether s parses as exactly
// scheme://host[:port] — no path, query, fragment, or userinfo. This is
// the shape a browser's Origin header always takes, so an allowlist
// entry in any other shape could never match one and is almost
// certainly a configuration mistake worth failing fast on.
func validOriginPattern(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme != "" && u.Host != "" && u.User == nil &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
