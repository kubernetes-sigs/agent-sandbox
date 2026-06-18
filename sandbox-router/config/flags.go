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

package config

import (
	"flag"
	"strconv"
	"strings"
	"time"
)

// Environment-variable aliases preserved from the original Python router so
// existing operators don't have to relearn the surface.
const (
	EnvClusterDomain = "CLUSTER_DOMAIN"
	EnvProxyTimeout  = "PROXY_TIMEOUT_SECONDS"
	// EnvKubeconfig matches the standard kubectl env var so deployments
	// can drop a kubeconfig file alongside the binary without an explicit
	// flag.
	EnvKubeconfig = "KUBECONFIG"

	// Standard OpenTelemetry exporter env vars. When any of these is set
	// and the corresponding --enable-* flag wasn't explicitly passed on
	// the command line, the relevant signal is auto-enabled. See
	// ApplyPostParseEnvDefaults.
	EnvOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
)

// LookupEnvFunc matches the signature of os.LookupEnv. Tests inject a fake.
type LookupEnvFunc func(string) (string, bool)

// RegisterFlags wires every flag the binary accepts into fs, using c's current
// values as defaults. Env-var fallbacks for the keys the Python router used
// (PROXY_TIMEOUT_SECONDS, CLUSTER_DOMAIN) are applied to c BEFORE flag
// registration so they show up as the default in --help output, and so
// explicit flags override env vars.
func RegisterFlags(fs *flag.FlagSet, c *Config, lookup LookupEnvFunc) {
	if lookup == nil {
		// Fall back to a no-op to keep the function total; main wires os.LookupEnv.
		lookup = func(string) (string, bool) { return "", false }
	}
	applyEnvDefaults(c, lookup)

	fs.StringVar(&c.HTTPAddr, "http-bind-address", c.HTTPAddr,
		"Address for the plain-HTTP proxy listener (set empty to disable).")
	fs.StringVar(&c.HTTPSAddr, "https-bind-address", c.HTTPSAddr,
		"Address for the TLS proxy listener (empty disables TLS).")
	fs.StringVar(&c.MetricsAddr, "metrics-bind-address", c.MetricsAddr,
		"Address for the Prometheus /metrics endpoint.")
	fs.StringVar(&c.ProbeAddr, "health-probe-bind-address", c.ProbeAddr,
		"Address for the /healthz and /readyz endpoints.")

	fs.StringVar(&c.TLSCertFile, "tls-cert-file", c.TLSCertFile,
		"Path to the PEM-encoded server certificate.")
	fs.StringVar(&c.TLSKeyFile, "tls-key-file", c.TLSKeyFile,
		"Path to the PEM-encoded server private key.")
	fs.StringVar(&c.TLSClientCAFile, "tls-client-ca-file", c.TLSClientCAFile,
		"Path to the PEM-encoded CA bundle used to verify client certificates "+
			"when --mtls-mode is optional or required.")
	stringEnumVar(fs, (*string)(&c.MTLSMode), "mtls-mode", string(c.MTLSMode),
		"Client-cert verification policy: off, optional, or required.")

	fs.StringVar(&c.ClusterDomain, "cluster-domain", c.ClusterDomain,
		"Kubernetes cluster DNS suffix used to build sandbox FQDNs. "+
			"Honors "+EnvClusterDomain+".")
	fs.DurationVar(&c.ProxyTimeout, "proxy-timeout", c.ProxyTimeout,
		"Total time budget for proxying a single request to a sandbox. "+
			"Honors "+EnvProxyTimeout+" (numeric seconds).")
	fs.DurationVar(&c.ResponseHeaderTimeout, "response-header-timeout", c.ResponseHeaderTimeout,
		"Maximum time to wait for the upstream response headers.")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", c.ShutdownTimeout,
		"Time budget for draining in-flight requests on SIGTERM.")
	fs.Int64Var(&c.MaxRequestBodyBytes, "max-request-body-bytes", c.MaxRequestBodyBytes,
		"Optional cap on inbound request body size in bytes. 0 means unlimited.")
	fs.BoolVar(&c.AllowLoopbackPodIP, "allow-loopback-pod-ip", c.AllowLoopbackPodIP,
		"Allow X-Sandbox-Pod-IP to carry a loopback address (127.0.0.0/8 or ::1). "+
			"Default false (loopback rejected with 400). Enable for sidecar "+
			"deployments where the sandbox shares a Pod with the router, or for "+
			"integration tests using a localhost backend. Link-local, multicast, "+
			"and unspecified addresses stay rejected regardless of this flag.")
	fs.IntVar(&c.UpstreamMaxRetries, "upstream-max-retries", c.UpstreamMaxRetries,
		"Number of additional dial attempts before returning 502. Only dial-class "+
			"failures (DNS, connection refused) are retried. Smooths the case "+
			"where a freshly-created sandbox is not yet ready. 0 disables retries.")
	fs.DurationVar(&c.UpstreamRetryInitialDelay, "upstream-retry-initial-delay", c.UpstreamRetryInitialDelay,
		"Wait before the first retry; subsequent waits double up to --upstream-retry-max-delay.")
	fs.DurationVar(&c.UpstreamRetryMaxDelay, "upstream-retry-max-delay", c.UpstreamRetryMaxDelay,
		"Upper bound on the per-iteration retry backoff.")

	fs.BoolVar(&c.EnableTracing, "enable-tracing", c.EnableTracing,
		"Enable OpenTelemetry tracing via OTLP. Endpoint is taken from "+
			"OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT). "+
			"Auto-enabled when either env var is set; pass --enable-tracing=false "+
			"to override.")
	fs.BoolVar(&c.EnableOTelMetrics, "enable-otel-metrics", c.EnableOTelMetrics,
		"Additionally push metrics via OTLP gRPC. The Prometheus /metrics "+
			"endpoint remains active. Endpoint is taken from "+
			"OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT). "+
			"Auto-enabled when either env var is set; pass --enable-otel-metrics=false "+
			"to override.")
	stringEnumVar(fs, (*string)(&c.AuthzMode), "authz-mode", string(c.AuthzMode),
		"Per-request authorization strategy: allow-all (default, no auth) "+
			"or tokenreview (validate Bearer tokens via the K8s TokenReview API). "+
			"tokenreview requires either in-cluster config or --kubeconfig.")
	fs.DurationVar(&c.AuthzTokenReviewTTL, "authz-tokenreview-ttl", c.AuthzTokenReviewTTL,
		"How long a TokenReview decision is cached. Shorter values catch "+
			"token revocations sooner at the cost of more apiserver load.")
	fs.IntVar(&c.AuthzTokenReviewCacheSize, "authz-tokenreview-cache-size", c.AuthzTokenReviewCacheSize,
		"Maximum number of cached TokenReview decisions before LRU eviction.")
	fs.BoolVar(&c.AuthzTokenReviewRequireToken, "authz-tokenreview-require-token", c.AuthzTokenReviewRequireToken,
		"When true, reject requests that arrive without an Authorization: "+
			"Bearer header with 401. When false (default), tokenless requests "+
			"are allowed — useful during client rollouts.")
	stringSliceVar(fs, &c.AuthzTokenReviewAudiences, "authz-tokenreview-audiences",
		"Comma-separated audience values to verify against the token's aud claim. "+
			"Empty disables the audience check. Required when authenticating "+
			"projected ServiceAccount tokens minted with --audience.")

	fs.BoolVar(&c.CacheEnabled, "cache-enabled", c.CacheEnabled,
		"Enable the in-process Pod-IP cache (KEP-NNNN fast path). When on, "+
			"the router watches sandbox-owned Pods and dials cached IPs for "+
			"requests carrying X-Sandbox-UID, bypassing DNS. Requires Pod "+
			"get/list/watch RBAC and either in-cluster config or --kubeconfig.")
	fs.StringVar(&c.CacheNamespace, "cache-namespace", c.CacheNamespace,
		"Optional namespace filter for the Pod informer. Empty means "+
			"cluster-wide. Ignored when --cache-enabled=false.")
	// controller-runtime's pkg/client/config registers a "kubeconfig"
	// flag in its package init. Detect that and reuse the existing
	// flag rather than redefining it (Go's flag package panics on
	// re-registration). We pull the value back into c.Kubeconfig in
	// ApplyPostParseEnvDefaults so the precedence (flag > file > env >
	// default) still holds.
	if fs.Lookup("kubeconfig") == nil {
		fs.StringVar(&c.Kubeconfig, "kubeconfig", c.Kubeconfig,
			"Path to a kubeconfig file used to build the informer client. "+
				"Empty means use in-cluster config. Honors "+EnvKubeconfig+".")
	}

	fs.BoolVar(&c.AccessLog, "access-log", c.AccessLog,
		"Emit one structured log line per inbound request on the proxy "+
			"port. Health/metrics endpoints are skipped.")
	fs.BoolVar(&c.PrintVersion, "version", c.PrintVersion,
		"Print version information and exit.")
	// --config is intentionally registered so it appears in --help, but the
	// file is actually loaded BEFORE flag.Parse runs (see main.go) by way of
	// FileFromArgsAndEnv. Setting it here a second time during parse
	// is harmless — the value is the same.
	fs.StringVar(&c.ConfigFile, "config", c.ConfigFile,
		"Path to a YAML config file with keys matching flag names "+
			"(kebab-case). Also honors "+EnvConfigFile+". File values override "+
			"env-var defaults; CLI flags override file values.")
}

// ApplyPostParseEnvDefaults turns on tracing and / or OTel metrics push
// when the corresponding OTLP endpoint env var is set AND the user did NOT
// explicitly pass --enable-tracing / --enable-otel-metrics on the command
// line.
//
// This MUST be called AFTER fs.Parse so we can distinguish "user did not
// set" (in which case env-driven auto-enable applies) from "user
// explicitly passed --enable-tracing=false" (in which case the explicit
// value wins). fs.Visit only iterates flags actually set on the command
// line, which is exactly the discrimination we need.
func ApplyPostParseEnvDefaults(fs *flag.FlagSet, c *Config, lookup LookupEnvFunc) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	setExplicitly := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setExplicitly[f.Name] = true })

	endpointSet := func(specific string) bool {
		if v, ok := lookup(specific); ok && v != "" {
			return true
		}
		if v, ok := lookup(EnvOTLPEndpoint); ok && v != "" {
			return true
		}
		return false
	}

	if !setExplicitly["enable-tracing"] && endpointSet(EnvOTLPTracesEndpoint) {
		c.EnableTracing = true
	}
	if !setExplicitly["enable-otel-metrics"] && endpointSet(EnvOTLPMetricsEndpoint) {
		c.EnableOTelMetrics = true
	}
	// When controller-runtime owns the --kubeconfig flag (because its
	// package init beat us to flag.CommandLine), we couldn't bind
	// c.Kubeconfig directly. Pull the post-parse value into c so the
	// rest of the code reads from a single field.
	//
	// fs.Visit was already consulted above to build setExplicitly, so
	// we know whether the flag was passed on the command line. Use
	// that to honor precedence: an explicit CLI override wins over any
	// value already in c (from a config file or env var). A non-empty
	// flag value that *wasn't* set on the command line is the parser's
	// zero — leave c alone.
	if f := fs.Lookup("kubeconfig"); f != nil {
		v := f.Value.String()
		if setExplicitly["kubeconfig"] {
			c.Kubeconfig = v // CLI wins, even when c.Kubeconfig is non-empty
		} else if c.Kubeconfig == "" && v != "" {
			c.Kubeconfig = v // no CLI override; pick up any default the flag carries
		}
	}
}

// applyEnvDefaults overrides c's defaults with values pulled from the
// environment. It is intentionally lenient: invalid env values are ignored
// (the existing default wins) so that operators don't get a hard crash from
// a typo in a deployment manifest.
func applyEnvDefaults(c *Config, lookup LookupEnvFunc) {
	if v, ok := lookup(EnvClusterDomain); ok && v != "" {
		c.ClusterDomain = v
	}
	if v, ok := lookup(EnvProxyTimeout); ok && v != "" {
		// The Python router accepts a numeric seconds value; preserve that.
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			c.ProxyTimeout = time.Duration(secs * float64(time.Second))
		}
	}
	if v, ok := lookup(EnvKubeconfig); ok && v != "" {
		c.Kubeconfig = v
	}
}

// stringEnumVar registers a string flag; the dedicated function exists so the
// flag help text uses a stable doc-style and to keep RegisterFlags scannable.
func stringEnumVar(fs *flag.FlagSet, dst *string, name, def, usage string) {
	fs.StringVar(dst, name, def, usage)
}

// stringSliceVar registers a flag whose value is a comma-separated list of
// strings. Empty input clears the slice; the default value (when the flag is
// not set) is whatever *dst already contains.
func stringSliceVar(fs *flag.FlagSet, dst *[]string, name, usage string) {
	fs.Var(&csvFlag{dst: dst}, name, usage)
}

// csvFlag is a flag.Value backed by a *[]string and a comma-separated
// surface syntax.
type csvFlag struct {
	dst *[]string
	set bool
}

func (c *csvFlag) String() string {
	if c == nil || c.dst == nil {
		return ""
	}
	return strings.Join(*c.dst, ",")
}

func (c *csvFlag) Set(v string) error {
	c.set = true
	if v == "" {
		*c.dst = nil
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	*c.dst = out
	return nil
}
