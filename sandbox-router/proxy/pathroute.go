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
	"net/url"
	"strconv"
	"strings"
)

// PathRoute is the result of successfully parsing a path-routed request.
type PathRoute struct {
	// Target is the resolved routing target — same type and same
	// validation ParseSandboxHeaders produces, regardless of which input
	// carried it.
	Target Target
	// UpstreamPath is the decoded remainder: the part of the path the
	// upstream sandbox should actually see, with
	// prefix/namespace/id/port stripped. Suitable for url.URL.Path.
	UpstreamPath string
	// UpstreamRawPath is the same remainder in the exact escaped form it
	// arrived in, preserving encoded separators like "%2F" that decoding
	// would otherwise silently collapse into a literal "/". Suitable for
	// url.URL.RawPath, set ALONGSIDE UpstreamPath — url.URL only honors
	// RawPath when it is a valid encoding of Path (see net/url's own
	// EscapedPath doc), so the two must be assigned as a pair, never
	// RawPath alone.
	UpstreamRawPath string
}

// ParsePathRoute extracts routing information from a request path shaped as
// <prefix>/<namespace>/<id>/<port>/<rest...>, where prefix is the operator's
// configured --path-routing-prefix and escapedPath is the request path
// exactly as it arrived on the wire (r.URL.EscapedPath(), NOT r.URL.Path —
// the latter is already percent-decoded, which would silently collapse an
// encoded separator like "%2F" inside the upstream remainder into a literal
// "/", changing which path segment a browser resource request actually
// names).
//
// matched reports whether escapedPath even starts with prefix — the caller
// is expected to fall through to header-based ParseSandboxHeaders when it
// is false, which is not an error, just "this is not a path-routed
// request". A perr is only ever returned alongside matched=true: the path
// opted in to this routing mode but is otherwise malformed (missing
// segments, bad namespace/id/port).
//
// The same validation as ParseSandboxHeaders applies to namespace and id
// (validDNSLabel) and to port ([1, 65535]) — one shape for a Target
// regardless of which input carried it. Those three segments are decoded
// before validation: a client is free to percent-encode a character that
// didn't strictly need it, and decoding first is the conventional way a
// server honors that — a DNS label or a decimal port never legitimately
// contains an encoded "/" in the first place, so this can't reintroduce
// the ambiguity splitting on the still-escaped string was written to
// avoid. Port has no default here (unlike the header form's
// DefaultSandboxPort): a browser-facing path with no port is an authoring
// mistake worth surfacing immediately, not silently guessing.
//
// X-Sandbox-Pod-IP and X-Sandbox-UID have no path equivalent, by design:
// see the PathRoutingPrefix doc comment in package config for why.
func ParsePathRoute(prefix, escapedPath string) (route PathRoute, matched bool, perr *Error) {
	if prefix == "" || !strings.HasPrefix(escapedPath, prefix) {
		return PathRoute{}, false, nil
	}
	rest := escapedPath[len(prefix):]
	// Require the leading slash explicitly, rather than accepting
	// "<prefix>something" as a match just because it happens to share a
	// string prefix with a sibling route the operator also serves.
	if !strings.HasPrefix(rest, "/") {
		return PathRoute{}, false, nil
	}

	// At most 4 parts: namespace, id, port, and everything after the
	// port. Splitting on literal "/" bytes in the STILL-ESCAPED string is
	// safe and unambiguous: an encoded separator ("%2F") is three ASCII
	// characters, never a literal "/", so it can't be mistaken for a
	// path-segment boundary here even if it showed up inside one of the
	// first three segments.
	parts := strings.SplitN(rest[1:], "/", 4)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return PathRoute{}, true, &Error{
			Status: http.StatusBadRequest,
			Detail: "Path-routed request must have the form <prefix>/<namespace>/<id>/<port>/...",
		}
	}

	ns, nsErr := url.PathUnescape(parts[0])
	id, idErr := url.PathUnescape(parts[1])
	rawPort, portErr := url.PathUnescape(parts[2])
	if nsErr != nil || idErr != nil || portErr != nil {
		return PathRoute{}, true, &Error{Status: http.StatusBadRequest, Detail: "Invalid percent-encoding in path."}
	}

	if !validDNSLabel(ns) {
		return PathRoute{}, true, &Error{Status: http.StatusBadRequest, Detail: "Invalid namespace format."}
	}
	if !validDNSLabel(id) {
		return PathRoute{}, true, &Error{Status: http.StatusBadRequest, Detail: "Invalid sandbox ID format."}
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return PathRoute{}, true, &Error{Status: http.StatusBadRequest, Detail: "Invalid port format."}
	}

	// Everything after the port is opaque to this router — NOT decoded,
	// preserved byte-for-byte, escaping included. That's what lets an
	// encoded separator inside a single upstream path segment (e.g. a
	// filename containing "/", sent as "%2F") survive the hop unchanged,
	// matching the same verbatim-remainder guarantee header-routed
	// requests already get simply by never touching r.URL.Path at all.
	rawRemainder := ""
	if len(parts) == 4 {
		rawRemainder = parts[3]
	}
	decodedRemainder, err := url.PathUnescape(rawRemainder)
	if err != nil {
		// Malformed escaping this deep in the path isn't this router's
		// business to reject — forward it as-is and let the upstream
		// sandbox decide. RawPath stays authoritative regardless: per
		// net/url's EscapedPath doc, RawPath is only honored when it is
		// a valid encoding of Path, so a mismatched Path here (the raw
		// text instead of a real decode) never actually gets used for
		// serialization — it's best-effort only, not load-bearing.
		decodedRemainder = rawRemainder
	}

	return PathRoute{
		Target:          Target{ID: id, Namespace: ns, Port: port},
		UpstreamPath:    "/" + decodedRemainder,
		UpstreamRawPath: "/" + rawRemainder,
	}, true, nil
}
