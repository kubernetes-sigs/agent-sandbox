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
	"net/http"
	"strconv"
	"strings"
)

// ParsePathRoute extracts routing information from a request path shaped as
// <prefix>/<namespace>/<id>/<port>/<rest...>, where prefix is the operator's
// configured --path-routing-prefix.
//
// matched reports whether path even starts with prefix — the caller is
// expected to fall through to header-based ParseSandboxHeaders when it is
// false, which is not an error, just "this is not a path-routed request".
// A perr is only ever returned alongside matched=true: the path opted in to
// this routing mode but is otherwise malformed (missing segments, bad
// namespace/id/port).
//
// On success, upstreamPath is what the upstream sandbox should actually
// see — prefix/namespace/id/port stripped, everything after preserved
// verbatim (matching the "verbatim remainder" contract header-routed
// requests already get for free, since they carry no prefix to strip).
//
// The same validation as ParseSandboxHeaders applies to namespace and id
// (validDNSLabel) and to port ([1, 65535]) — one shape for a Target
// regardless of which input carried it. Port has no default here (unlike
// the header form's DefaultSandboxPort): a browser-facing path with no
// port is an authoring mistake worth surfacing immediately, not silently
// guessing.
//
// X-Sandbox-Pod-IP and X-Sandbox-UID have no path equivalent, by design:
// see the PathRoutingPrefix doc comment in package config for why.
func ParsePathRoute(prefix, path string) (target Target, upstreamPath string, matched bool, perr *Error) {
	if prefix == "" || !strings.HasPrefix(path, prefix) {
		return Target{}, "", false, nil
	}
	rest := path[len(prefix):]
	// Require the leading slash explicitly, rather than accepting
	// "<prefix>something" as a match just because it happens to share a
	// string prefix with a sibling route the operator also serves.
	if !strings.HasPrefix(rest, "/") {
		return Target{}, "", false, nil
	}

	// At most 4 parts: namespace, id, port, and everything after the
	// port — which may itself contain slashes and must be preserved
	// verbatim.
	parts := strings.SplitN(rest[1:], "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Target{}, "", true, &Error{
			Status: http.StatusBadRequest,
			Detail: "Path-routed request must have the form <prefix>/<namespace>/<id>/<port>/...",
		}
	}
	ns, id, rawPort := parts[0], parts[1], parts[2]

	if !validDNSLabel(ns) {
		return Target{}, "", true, &Error{Status: http.StatusBadRequest, Detail: "Invalid namespace format."}
	}
	if !validDNSLabel(id) {
		return Target{}, "", true, &Error{Status: http.StatusBadRequest, Detail: "Invalid sandbox ID format."}
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return Target{}, "", true, &Error{Status: http.StatusBadRequest, Detail: "Invalid port format."}
	}

	upstreamPath = "/"
	if len(parts) == 4 {
		upstreamPath += parts[3]
	}
	return Target{ID: id, Namespace: ns, Port: port}, upstreamPath, true, nil
}
