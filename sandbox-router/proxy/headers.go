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

// Package proxy implements the request-routing logic of the sandbox-router.
package proxy

import (
	"net"
	"net/http"
	"strconv"
)

// Header names the router consumes. Kept exported so tests and downstream
// integrations have a single source of truth.
const (
	HeaderSandboxID        = "X-Sandbox-Id"
	HeaderSandboxUID       = "X-Sandbox-Uid"
	HeaderSandboxNamespace = "X-Sandbox-Namespace"
	HeaderSandboxPort      = "X-Sandbox-Port"
	HeaderSandboxPodIP     = "X-Sandbox-Pod-Ip"
)

// Defaults preserved from the Python router.
const (
	DefaultSandboxNamespace = "default"
	DefaultSandboxPort      = 8888
)

// Target describes the upstream sandbox a single request should be routed to.
type Target struct {
	// ID is the sandbox identifier from X-Sandbox-ID. Used as the host
	// component of the DNS form (and as a free-form label in logs/traces).
	ID string
	// UID is the Sandbox CR UID from X-Sandbox-UID. When the proxy is
	// running with a Pod informer cache attached, this is the key used to
	// look up the live PodIP — bypassing DNS resolution for the fast
	// secure path described in KEP-NNNN. Empty when the client did not
	// supply the header; DNS-form routing still works.
	UID string
	// Namespace is the Kubernetes namespace of the sandbox.
	Namespace string
	// Port is the upstream port.
	Port int
	// PodIP is the optional direct pod IP from X-Sandbox-Pod-IP. When set,
	// both DNS and cache lookups are bypassed and the proxy dials this IP
	// directly. Lets a caller (typically an SDK that just created the
	// Sandbox) skip the discovery hop entirely.
	PodIP string
}

// ParseOptions controls validation behaviors that need to differ
// between production and certain self-loopback deployments. Adding new
// knobs in a struct rather than as positional args keeps the call
// site readable as the validation surface grows.
type ParseOptions struct {
	// AllowLoopbackPodIP, when true, lets a loopback address in
	// X-Sandbox-Pod-IP pass validation. See config.Config for the
	// production reasoning.
	AllowLoopbackPodIP bool
}

// ParseSandboxHeaders extracts and validates the routing headers from h.
// On any validation failure it returns a non-nil *Error with the same
// status codes and detail-message shape as the Python router.
func ParseSandboxHeaders(h http.Header, opts ParseOptions) (Target, *Error) {
	id := h.Get(HeaderSandboxID)
	if id == "" {
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "X-Sandbox-ID header is required."}
	}
	// DNS-label validation prevents DNS injection and traversal-style
	// inputs from being interpolated into the upstream FQDN
	// "<id>.<ns>.svc.<cluster-domain>". Matches the Python router's
	// _is_valid_dns_label check.
	if !validDNSLabel(id) {
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid sandbox ID format."}
	}

	ns := h.Get(HeaderSandboxNamespace)
	if ns == "" {
		ns = DefaultSandboxNamespace
	}
	if !validDNSLabel(ns) {
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid namespace format."}
	}

	port := DefaultSandboxPort
	if raw := h.Get(HeaderSandboxPort); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid port format."}
		}
		// TCP port range is [1, 65535]. Reject anything outside it
		// before it can ride into the upstream URL — an out-of-range
		// value would round-trip to net.JoinHostPort and produce a
		// syntactically valid but semantically junk host:port that
		// surfaces downstream as an opaque 502.
		if n < 1 || n > 65535 {
			return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid port format."}
		}
		port = n
	}

	podIP := h.Get(HeaderSandboxPodIP)
	if podIP != "" && !validPodIP(podIP, opts.AllowLoopbackPodIP) {
		// validPodIP folds the parse + class check into one decision.
		// We use the same 400 message regardless of parse-vs-class so
		// callers don't get to probe the boundary between "looks like
		// an IP" and "is a routable IP".
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid target IP address."}
	}

	return Target{
		ID:        id,
		UID:       h.Get(HeaderSandboxUID),
		Namespace: ns,
		Port:      port,
		PodIP:     podIP,
	}, nil
}

// validDNSLabel reports whether s is a syntactically valid DNS-1123
// label (RFC 1123). Mirrors the Python router's
//
//	^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$
//
// regex with a 63-character cap. Applied to both X-Sandbox-ID and
// X-Sandbox-Namespace before either gets interpolated into the
// upstream FQDN, so callers can't inject extra DNS components or
// traversal sequences ("foo.evil.com", "..", "foo/bar", etc.).
//
// Note this is intentionally stricter than the previous
// validNamespace: it rejects uppercase letters, leading/trailing
// hyphens, and anything over 63 chars. K8s itself enforces the same
// rule on Namespace and Pod names, so any sandbox we'd actually want
// to route to already conforms.
func validDNSLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c == '-':
			// Hyphen is allowed only in the interior — not first or last.
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validPodIP reports whether s is a syntactically valid IP literal AND
// belongs to a routable address class. We reject the IP classes the
// Python router rejects — loopback, link-local (unicast and multicast,
// covers IPv6 fe80::/10 as well as IPv4 169.254.0.0/16, which
// importantly blocks cloud metadata endpoints like 169.254.169.254),
// multicast, and the unspecified address. These are not valid Pod IPs
// and accepting them as X-Sandbox-Pod-IP would turn the router into
// an SSRF gadget — letting a caller dial cluster-internal admin
// endpoints, the loopback of the router pod itself, or cloud
// instance-metadata services.
//
// We do NOT additionally restrict to "looks like a Pod CIDR" because
// the SDK fast-path (caller just created the Sandbox, knows its IP)
// is a supported use case and Pod CIDRs vary per cluster.
//
// allowLoopback exists for the sidecar deployment shape (sandbox in
// the same Pod as the router) and for integration tests using a
// localhost httptest backend. Even with allowLoopback=true the other
// IP classes (link-local, multicast, unspecified) stay rejected —
// nothing legitimate would use those as a Pod IP regardless of the
// deployment shape.
func validPodIP(s string, allowLoopback bool) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	if ip.IsLoopback() && !allowLoopback {
		return false
	}
	return true
}
