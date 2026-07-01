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

// Package config defines the runtime configuration for the sandbox-router binary.
package config

import (
	"errors"
	"fmt"
	"time"
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

// AuthzMode selects the per-request authorization strategy.
type AuthzMode string

const (
	// AuthzAllowAll permits every request (Python router default).
	AuthzAllowAll AuthzMode = "allow-all"
	// AuthzTokenReview authenticates each request via the K8s
	// TokenReview API. Per-sandbox authorization beyond authentication
	// is out of v1 scope.
	AuthzTokenReview AuthzMode = "tokenreview"
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
	case AuthzAllowAll, AuthzTokenReview:
	default:
		return fmt.Errorf("invalid --authz-mode %q (want allow-all or tokenreview)", c.AuthzMode)
	}
	if c.AuthzTokenReviewTTL <= 0 {
		return fmt.Errorf("--authz-tokenreview-ttl must be positive, got %s", c.AuthzTokenReviewTTL)
	}
	if c.AuthzTokenReviewCacheSize <= 0 {
		return fmt.Errorf("--authz-tokenreview-cache-size must be positive, got %d", c.AuthzTokenReviewCacheSize)
	}
	return nil
}
