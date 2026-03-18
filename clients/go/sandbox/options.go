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

package sandbox

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/rest"
)

const (
	defaultNamespace               = "default"
	defaultServerPort              = 8888
	defaultSandboxReadyTimeout     = 180 * time.Second
	defaultGatewayReadyTimeout     = 180 * time.Second
	defaultPortForwardReadyTimeout = 30 * time.Second
	defaultCleanupTimeout          = 30 * time.Second
	defaultRequestTimeout          = 180 * time.Second
	defaultPerAttemptTimeout       = 60 * time.Second
	defaultMaxDownloadSize         = 256 << 20 // 256 MB
	defaultMaxUploadSize           = 256 << 20 // 256 MB
)

// Options configures a Sandbox instance.
type Options struct {
	// TemplateName is the name of the SandboxTemplate to use. Required.
	// Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
	TemplateName string

	// Namespace where the SandboxClaim will be created. Default: "default".
	// Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
	Namespace string

	// GatewayName enables production mode. The client watches this Gateway resource
	// for an external IP, then routes through the sandbox-router.
	// Must be a valid Kubernetes DNS subdomain (lowercase, [a-z0-9.-]).
	GatewayName string

	// GatewayNamespace is where the Gateway lives. Default: "default".
	// Must be a valid Kubernetes DNS label (lowercase, [a-z0-9-]).
	GatewayNamespace string

	// GatewayScheme is the URL scheme used when constructing the base URL
	// from the Gateway's address. Default: "http".
	GatewayScheme string

	// APIURL enables advanced mode. The client connects directly to this URL,
	// bypassing gateway discovery. Takes precedence over GatewayName.
	APIURL string

	// ServerPort is the port the sandbox runtime listens on. Default: 8888.
	ServerPort int

	// SandboxReadyTimeout is how long to wait for the sandbox to become ready. Default: 180s.
	SandboxReadyTimeout time.Duration

	// GatewayReadyTimeout is how long to wait for the gateway IP. Default: 180s.
	GatewayReadyTimeout time.Duration

	// PortForwardReadyTimeout is how long to wait for port-forward to be established. Default: 30s.
	PortForwardReadyTimeout time.Duration

	// CleanupTimeout is how long to wait for claim deletion during both Open
	// rollback and Close. Uses a detached context so cleanup succeeds even if
	// the caller's context is already cancelled. Default: 30s.
	CleanupTimeout time.Duration

	// RequestTimeout is the total timeout for a single SDK method call
	// (e.g., Run, Read, Write), encompassing all retry attempts and backoff
	// sleeps. Applied only when the caller's context has no deadline.
	// Default: 180s.
	RequestTimeout time.Duration

	// PerAttemptTimeout bounds the time to receive response headers per
	// HTTP attempt. Stopped on success so body reads use RequestTimeout.
	// Default: 60s.
	PerAttemptTimeout time.Duration

	// MaxDownloadSize is the maximum response body size for Read().
	// Run() uses a fixed 16 MB decode limit; List() and Exists() use a
	// fixed 8 MB internal limit. Default: 256 MB.
	MaxDownloadSize int64

	// MaxUploadSize is the maximum content size for Write(). Content
	// exceeding this limit is rejected before any network I/O. Default: 256 MB.
	MaxUploadSize int64

	// Logger for structured logging. Defaults to stderr at INFO level.
	// Provide a custom logr.Logger for full control, or set Quiet to
	// suppress output.
	Logger logr.Logger

	// Quiet suppresses the default stderr logger. Has no effect when a
	// custom Logger is provided (non-zero-value).
	Quiet bool

	// K8sHelper provides pre-constructed Kubernetes clients. If nil, a new
	// K8sHelper is created from RestConfig. Use this to share clients
	// across multiple Sandbox instances.
	K8sHelper *K8sHelper

	// RestConfig overrides the Kubernetes REST config. If nil, the client first
	// tries in-cluster config (for pods), then falls back to the default
	// kubeconfig (~/.kube/config or KUBECONFIG env). Ignored when K8sHelper is set.
	RestConfig *rest.Config

	// HTTPTransport overrides the HTTP transport for sandbox operations.
	// If nil, a default transport with sensible timeouts is created.
	// Use this for custom TLS, proxies, or other transport-level settings.
	HTTPTransport http.RoundTripper

	// EnableTracing auto-initializes a global OTLP/gRPC TracerProvider when
	// no custom TracerProvider is supplied. The exporter endpoint is controlled
	// by OTEL_EXPORTER_OTLP_ENDPOINT (default: localhost:4317). Default: false.
	EnableTracing bool

	// TraceServiceName is the OpenTelemetry service name used for the tracer's
	// instrumentation scope and the resource's service.name attribute.
	// Default: "sandbox-client".
	TraceServiceName string

	// TracerProvider overrides the OpenTelemetry TracerProvider used for span
	// creation. If nil and EnableTracing is false, the global provider is used
	// (noop by default). If nil and EnableTracing is true, a provider with an
	// OTLP/gRPC exporter is auto-initialized.
	TracerProvider trace.TracerProvider
}

func (o *Options) setDefaults() {
	if o.Namespace == "" {
		o.Namespace = defaultNamespace
	}
	if o.GatewayNamespace == "" {
		o.GatewayNamespace = defaultNamespace
	}
	if o.GatewayScheme == "" {
		o.GatewayScheme = "http"
	}
	if o.ServerPort == 0 {
		o.ServerPort = defaultServerPort
	}
	if o.SandboxReadyTimeout == 0 {
		o.SandboxReadyTimeout = defaultSandboxReadyTimeout
	}
	if o.GatewayReadyTimeout == 0 {
		o.GatewayReadyTimeout = defaultGatewayReadyTimeout
	}
	if o.PortForwardReadyTimeout == 0 {
		o.PortForwardReadyTimeout = defaultPortForwardReadyTimeout
	}
	if o.CleanupTimeout == 0 {
		o.CleanupTimeout = defaultCleanupTimeout
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = defaultRequestTimeout
	}
	if o.PerAttemptTimeout == 0 {
		o.PerAttemptTimeout = defaultPerAttemptTimeout
	}
	if o.MaxDownloadSize == 0 {
		o.MaxDownloadSize = defaultMaxDownloadSize
	}
	if o.MaxUploadSize == 0 {
		o.MaxUploadSize = defaultMaxUploadSize
	}
	if o.TraceServiceName == "" {
		o.TraceServiceName = "sandbox-client"
	}
	if o.Logger.GetSink() == nil {
		if o.Quiet {
			o.Logger = logr.Discard()
		} else {
			o.Logger = funcr.New(func(prefix, args string) {
				if prefix != "" {
					fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, args)
				} else {
					fmt.Fprintln(os.Stderr, args)
				}
			}, funcr.Options{LogTimestamp: true})
		}
	}
}

// isValidDNSSubdomain checks RFC 1123 DNS subdomain rules: max 253 chars,
// dot-separated labels of [a-z0-9-] that start and end with alphanumeric.
// Per-label length is not enforced here because Kubernetes IsDNS1123Subdomain
// does not enforce it for resource names.
func isValidDNSSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 || s[i-1] == '.' {
				return false
			}
		case c == '.':
			if i == 0 || i == len(s)-1 || s[i-1] == '.' || s[i-1] == '-' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isValidDNSLabel checks RFC 1123 DNS label rules used for K8s namespaces:
// max 63, [a-z0-9-], no dots.
func isValidDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if c == '-' && i > 0 && i < len(s)-1 {
			continue
		}
		return false
	}
	return true
}

func (o *Options) validate() error {
	if o.APIURL != "" {
		u, err := url.Parse(o.APIURL)
		if err != nil {
			return fmt.Errorf("sandbox: APIURL %q is not a valid URL: %w", o.APIURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("sandbox: APIURL must have http or https scheme, got %q", o.APIURL)
		}
		if u.Host == "" {
			return fmt.Errorf("sandbox: APIURL %q must include a host", o.APIURL)
		}
	}
	if o.TemplateName == "" {
		return fmt.Errorf("sandbox: TemplateName is required")
	}
	if !isValidDNSSubdomain(o.TemplateName) {
		return fmt.Errorf("sandbox: TemplateName %q is not a valid Kubernetes DNS subdomain name", o.TemplateName)
	}
	if !isValidDNSLabel(o.Namespace) {
		return fmt.Errorf("sandbox: Namespace %q is not a valid Kubernetes namespace (DNS label)", o.Namespace)
	}
	if !isValidDNSLabel(o.GatewayNamespace) {
		return fmt.Errorf("sandbox: GatewayNamespace %q is not a valid Kubernetes namespace (DNS label)", o.GatewayNamespace)
	}
	if o.GatewayName != "" && !isValidDNSSubdomain(o.GatewayName) {
		return fmt.Errorf("sandbox: GatewayName %q is not a valid Kubernetes DNS subdomain name", o.GatewayName)
	}
	if o.GatewayScheme != "http" && o.GatewayScheme != "https" {
		return fmt.Errorf("sandbox: GatewayScheme must be \"http\" or \"https\", got %q", o.GatewayScheme)
	}
	if o.ServerPort <= 0 || o.ServerPort > 65535 {
		return fmt.Errorf("sandbox: ServerPort must be between 1 and 65535, got %d", o.ServerPort)
	}
	if o.SandboxReadyTimeout <= 0 {
		return fmt.Errorf("sandbox: SandboxReadyTimeout must be positive")
	}
	if o.GatewayReadyTimeout <= 0 {
		return fmt.Errorf("sandbox: GatewayReadyTimeout must be positive")
	}
	if o.PortForwardReadyTimeout <= 0 {
		return fmt.Errorf("sandbox: PortForwardReadyTimeout must be positive")
	}
	if o.CleanupTimeout <= 0 {
		return fmt.Errorf("sandbox: CleanupTimeout must be positive")
	}
	if o.RequestTimeout <= 0 {
		return fmt.Errorf("sandbox: RequestTimeout must be positive")
	}
	if o.PerAttemptTimeout <= 0 {
		return fmt.Errorf("sandbox: PerAttemptTimeout must be positive")
	}
	if o.MaxDownloadSize <= 0 {
		return fmt.Errorf("sandbox: MaxDownloadSize must be positive")
	}
	if o.MaxUploadSize <= 0 {
		return fmt.Errorf("sandbox: MaxUploadSize must be positive")
	}
	return nil
}
