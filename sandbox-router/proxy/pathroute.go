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
	"net"
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
		// business to reject — let the upstream sandbox see it and decide.
		// This is NOT a byte-for-byte passthrough, though: rawRemainder
		// isn't validly encoded, so url.URL.EscapedPath() can't use
		// UpstreamRawPath as-is (per its own doc, RawPath is only honored
		// when it decodes back to Path) and falls back to re-escaping
		// UpstreamPath from scratch instead — so a literal "%" the client
		// sent gets percent-encoded itself. A client-sent ".../my-box%ZZ"
		// reaches the upstream as ".../my-box%25ZZ", not "%ZZ" unchanged.
		decodedRemainder = rawRemainder
	}

	return PathRoute{
		Target:          Target{ID: id, Namespace: ns, Port: port},
		UpstreamPath:    "/" + decodedRemainder,
		UpstreamRawPath: "/" + rawRemainder,
	}, true, nil
}

// rewriteLocation maps an upstream redirect target back under base, the
// path-routing prefix of the sandbox that issued it
// (<prefix>/<namespace>/<id>/<port>), so a browser following the redirect
// stays addressed to the same sandbox. Only needed when the upstream does
// not know the prefix: an app that sees the prefixed path itself already
// emits prefixed Locations, and rewriting those again would double it.
//
// An absolute Location is rewritten only when isSelf reports its host as the
// sandbox or the router. Apps behind forwarded headers (Django, Rails,
// Flask, Grafana) build absolute redirects from the router's host, not the
// pod's, so both must count as self. Any other absolute URL is an external
// redirect and passes through unchanged. Relative references are left alone:
// the browser already resolves them against the prefixed request path.
func rewriteLocation(loc, base string, isSelf func(host string) bool) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		if !isSelf(u.Host) {
			return loc
		}
	} else if u.Host != "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		return loc
	}
	p := u.EscapedPath()
	if p == "" {
		// "http://host?x=1" has an empty path, which is the root once it
		// sits under the prefix.
		p = "/"
	}
	if p != base && !strings.HasPrefix(p, base+"/") {
		p = base + p
	}
	out := p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

// selfHostMatcher reports whether a Location host names the sandbox at
// upstreamHost or the router at routerHost. A port-less host is still the
// sandbox ("http://10.0.0.1/a" for an upstream dialed on :8888), and so is one
// on the upstream's own port. A host with a different explicit port is some
// other service on the pod, so it passes through unchanged. The router is
// matched by name alone, so a router reached on another port still counts.
func selfHostMatcher(routerHost, upstreamHost string) func(host string) bool {
	upName, upPort := splitHostLoose(upstreamHost)
	routerName, _ := splitHostLoose(routerHost)
	return func(host string) bool {
		name, port := splitHostLoose(host)
		if name == "" {
			return false
		}
		if strings.EqualFold(name, upName) && (port == "" || port == upPort) {
			return true
		}
		return routerName != "" && strings.EqualFold(name, routerName)
	}
}

// splitHostLoose splits host into its name and port. Unlike net.SplitHostPort
// it accepts a bare name with no port, and it unwraps an IPv6 literal's
// brackets in either case.
func splitHostLoose(host string) (name, port string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return strings.Trim(host, "[]"), ""
}

// rewriteSetCookiePath rescopes every Path attribute of one upstream
// Set-Cookie value under base, so the browser attaches the cookie only to
// requests addressed to this sandbox. Every attribute is rewritten, not just
// the first: browsers use the last Path they see, so a leftover "Path=/"
// would still win. A cookie with no Path attribute gets one: the browser's
// default-path is the directory of the request URL, and for a request to the
// bare base path (no trailing slash) that directory is the namespace, which
// every sandbox in it shares.
//
// This is collision avoidance, not isolation. Sandboxes still share one
// origin, so the prefix does not keep one sandbox's script from reading
// another's cookie.
func rewriteSetCookiePath(v, base string) string {
	// A __Host- cookie must be sent with Path=/ (RFC 6265bis §4.1.3.2.2), or
	// the browser rejects it. Rescoping it would break a working cookie, so it
	// is passed through; such a cookie already covers the prefixed URL.
	if name, _, _ := strings.Cut(v, "="); strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "__host-") {
		return v
	}
	parts := strings.Split(v, ";")
	hasPath := false
	for i := 1; i < len(parts); i++ {
		name, val, ok := strings.Cut(strings.TrimSpace(parts[i]), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "path") {
			continue
		}
		hasPath = true
		p := strings.TrimSpace(val)
		if !strings.HasPrefix(p, "/") || p == base || strings.HasPrefix(p, base+"/") {
			continue
		}
		parts[i] = " Path=" + base + p
	}
	if !hasPath {
		parts = append(parts, " Path="+base+"/")
	}
	return strings.Join(parts, ";")
}

// rewriteResponseHeaders applies rewriteSetCookiePath to an upstream response's
// Set-Cookie headers and, when rewriteLocations is set, rewriteLocation to its
// Location. It only touches those two headers; response bodies are passed
// through, so this never buffers a body.
func rewriteResponseHeaders(h http.Header, base string, isSelf func(host string) bool, rewriteLocations bool) {
	if loc := h.Get("Location"); rewriteLocations && loc != "" {
		h.Set("Location", rewriteLocation(loc, base, isSelf))
	}
	if vs := h.Values("Set-Cookie"); len(vs) > 0 {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = rewriteSetCookiePath(v, base)
		}
		h["Set-Cookie"] = out
	}
}
