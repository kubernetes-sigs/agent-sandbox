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

// ParseSandboxHeaders extracts and validates the routing headers from h.
// On any validation failure it returns a non-nil *Error with the same
// status codes and detail-message shape as the Python router.
func ParseSandboxHeaders(h http.Header) (Target, *Error) {
	id := h.Get(HeaderSandboxID)
	if id == "" {
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "X-Sandbox-ID header is required."}
	}

	ns := h.Get(HeaderSandboxNamespace)
	if ns == "" {
		ns = DefaultSandboxNamespace
	}
	if !validNamespace(ns) {
		return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid namespace format."}
	}

	port := DefaultSandboxPort
	if raw := h.Get(HeaderSandboxPort); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Target{}, &Error{Status: http.StatusBadRequest, Detail: "Invalid port format."}
		}
		port = n
	}

	return Target{
		ID:        id,
		UID:       h.Get(HeaderSandboxUID),
		Namespace: ns,
		Port:      port,
		PodIP:     h.Get(HeaderSandboxPodIP),
	}, nil
}

// validNamespace mirrors the Python router's check
//
//	namespace.replace("-", "").isalnum()
//
// in pure ASCII terms: at least one alphanumeric character, and only ASCII
// letters/digits/hyphens otherwise. Empty input and hyphen-only input are
// both rejected.
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
