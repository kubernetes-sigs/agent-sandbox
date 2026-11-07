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
	"testing"
)

func TestParsePathRoute(t *testing.T) {
	cases := []struct {
		name            string
		prefix          string
		path            string
		wantMatched     bool
		wantCode        int // 0 means success (or "not matched", see wantMatched)
		wantTarget      Target
		wantUpstream    string
		wantUpstreamRaw string
	}{
		{
			name:        "prefix disabled never matches",
			prefix:      "",
			path:        "/router/test/my-box/8080/",
			wantMatched: false,
		},
		{
			name:        "path outside the prefix does not match",
			prefix:      "/router",
			path:        "/other/test/my-box/8080/",
			wantMatched: false,
		},
		{
			name:        "prefix as bare string-prefix of a sibling path does not match",
			prefix:      "/router",
			path:        "/routerish/test/my-box/8080/",
			wantMatched: false,
		},
		{
			name:            "happy path, no trailing content",
			prefix:          "/router",
			path:            "/router/test/my-box/8080",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
		{
			name:            "happy path, trailing slash only",
			prefix:          "/router",
			path:            "/router/test/my-box/8080/",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
		{
			name:            "happy path, nested remainder preserved verbatim",
			prefix:          "/router",
			path:            "/router/test/my-box/8080/stable-abc/static/out/vs/code.js",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream:    "/stable-abc/static/out/vs/code.js",
			wantUpstreamRaw: "/stable-abc/static/out/vs/code.js",
		},
		{
			// Regression test for the escaping bug CodeRabbit's review
			// caught: r.URL.Path decodes "%2F" to a literal "/" before
			// ParsePathRoute ever sees it, which would silently turn one
			// path segment (e.g. a filename containing "/") into two.
			// Operating on the still-escaped path (r.URL.EscapedPath())
			// keeps "%2F" as three literal characters, not a delimiter.
			name:            "encoded slash in the remainder is preserved, not split on",
			prefix:          "/router",
			path:            "/router/test/my-box/8080/some%2Ffile",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream:    "/some/file",   // decoded form, for url.URL.Path
			wantUpstreamRaw: "/some%2Ffile", // escaped form, for url.URL.RawPath — this is the one that actually reaches the wire
		},
		{
			// A client is free to percent-encode a namespace/id character
			// that didn't strictly need it (RFC 3986 allows this) — the
			// router decodes before validating, same as it would for any
			// other path segment it interprets rather than forwards.
			name:            "harmlessly over-escaped namespace and id still validate",
			prefix:          "/router",
			path:            "/router/te%73t/my%2Dbox/8080/",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
		{
			name:        "missing namespace and id rejected",
			prefix:      "/router",
			path:        "/router/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "missing port rejected",
			prefix:      "/router",
			path:        "/router/test/my-box",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "missing port with trailing slash rejected",
			prefix:      "/router",
			path:        "/router/test/my-box/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "invalid namespace rejected",
			prefix:      "/router",
			path:        "/router/BAD_NS/my-box/8080/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "invalid id rejected (dot would inject a DNS component)",
			prefix:      "/router",
			path:        "/router/test/foo.evil.com/8080/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			// Decoding "foo%2Eevil%2Ecom" produces "foo.evil.com", which
			// validDNSLabel rejects exactly like the unescaped case above
			// — decoding before validating must not open a bypass.
			name:        "percent-encoded dot in id still rejected after decoding",
			prefix:      "/router",
			path:        "/router/test/foo%2Eevil%2Ecom/8080/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			// url.PathUnescape fails on a malformed escape ("%ZZ" is not
			// valid hex) — exercises the id branch of that error path.
			// namespace and port share the same handling, so one case
			// covering the mechanism is enough; this isn't a per-field
			// property to re-verify three times over.
			name:        "malformed percent-encoding rejected",
			prefix:      "/router",
			path:        "/router/test/my-box%ZZ/8080/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "non-numeric port rejected",
			prefix:      "/router",
			path:        "/router/test/my-box/abc/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "zero port rejected",
			prefix:      "/router",
			path:        "/router/test/my-box/0/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:        "port above 65535 rejected",
			prefix:      "/router",
			path:        "/router/test/my-box/65536/",
			wantMatched: true,
			wantCode:    http.StatusBadRequest,
		},
		{
			name:            "port 1 accepted",
			prefix:          "/router",
			path:            "/router/test/my-box/1/",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 1},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
		{
			name:            "port 65535 accepted",
			prefix:          "/router",
			path:            "/router/test/my-box/65535/",
			wantMatched:     true,
			wantTarget:      Target{ID: "my-box", Namespace: "test", Port: 65535},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
		{
			name:            "root-mounted prefix",
			prefix:          "/sandboxes",
			path:            "/sandboxes/poc-agent-sandbox/sandbox-abc123/4200/",
			wantMatched:     true,
			wantTarget:      Target{ID: "sandbox-abc123", Namespace: "poc-agent-sandbox", Port: 4200},
			wantUpstream:    "/",
			wantUpstreamRaw: "/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, matched, perr := ParsePathRoute(tc.prefix, tc.path)
			if matched != tc.wantMatched {
				t.Fatalf("matched: got %v, want %v (route=%+v, perr=%v)", matched, tc.wantMatched, route, perr)
			}
			if !matched {
				return // nothing else to check — caller falls through to headers
			}
			if tc.wantCode != 0 {
				if perr == nil {
					t.Fatalf("expected error, got route=%+v", route)
				}
				if perr.Status != tc.wantCode {
					t.Fatalf("status: got %d, want %d (detail=%q)", perr.Status, tc.wantCode, perr.Detail)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if route.Target != tc.wantTarget {
				t.Fatalf("target: got %+v, want %+v", route.Target, tc.wantTarget)
			}
			if route.UpstreamPath != tc.wantUpstream {
				t.Fatalf("UpstreamPath: got %q, want %q", route.UpstreamPath, tc.wantUpstream)
			}
			if route.UpstreamRawPath != tc.wantUpstreamRaw {
				t.Fatalf("UpstreamRawPath: got %q, want %q", route.UpstreamRawPath, tc.wantUpstreamRaw)
			}
		})
	}
}

func TestRewriteLocation(t *testing.T) {
	const base = "/r/ns/id/8888"
	const upstream = "10.0.0.1:8888"
	cases := []struct {
		name string
		loc  string
		want string
	}{
		{"absolute path gets prefix", "/login", base + "/login"},
		{"root path maps to base slash", "/", base + "/"},
		{"query and fragment survive", "/login?next=/a#top", base + "/login?next=/a#top"},
		{"already prefixed is unchanged", base + "/x", base + "/x"},
		{"exact base is unchanged", base, base},
		{"sibling sharing a string prefix is prefixed", "/r/ns/id/8888x", base + "/r/ns/id/8888x"},
		{"relative reference is unchanged", "login", "login"},
		{"protocol-relative is unchanged", "//evil.example/x", "//evil.example/x"},
		{"external absolute is unchanged", "https://ext.example/x", "https://ext.example/x"},
		{"absolute to the upstream host is rewritten", "http://10.0.0.1:8888/a", base + "/a"},
		{"absolute to another host is unchanged", "http://10.0.0.2:8888/a", "http://10.0.0.2:8888/a"},
		{"empty path maps to base slash", "http://10.0.0.1:8888", base + "/"},
		{"empty path keeps query", "http://10.0.0.1:8888?x=1", base + "/?x=1"},
		{"upstream host without port is still self", "http://10.0.0.1/a", base + "/a"},
		{"router host is self (forwarded-host redirect)", "https://router.example:8080/login", base + "/login"},
		{"router host without port is self", "https://router.example/login", base + "/login"},
		{"external host with router-like name is unchanged", "https://router.example.evil/x", "https://router.example.evil/x"},
	}
	isSelf := selfHostMatcher("router.example:8080", upstream)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteLocation(tc.loc, base, isSelf); got != tc.want {
				t.Fatalf("rewriteLocation(%q) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}
}

func TestRewriteSetCookiePath(t *testing.T) {
	const base = "/r/ns/id/8888"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root path rescoped", "sid=1; Path=/; HttpOnly", "sid=1; Path=/r/ns/id/8888/; HttpOnly"},
		{"lowercase attribute rescoped", "sid=1; path=/lab", "sid=1; Path=/r/ns/id/8888/lab"},
		{"other attributes preserved", "sid=1; Domain=x; Path=/; Secure", "sid=1; Domain=x; Path=/r/ns/id/8888/; Secure"},
		{"already prefixed is unchanged", "sid=1; Path=/r/ns/id/8888/x", "sid=1; Path=/r/ns/id/8888/x"},
		{"no path attribute is unchanged", "sid=1; HttpOnly", "sid=1; HttpOnly"},
		{"relative path is unchanged", "sid=1; Path=lab", "sid=1; Path=lab"},
		{"every Path attribute is rewritten", "sid=1; Path=/; Path=/lab", "sid=1; Path=/r/ns/id/8888/; Path=/r/ns/id/8888/lab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteSetCookiePath(tc.in, base); got != tc.want {
				t.Fatalf("rewriteSetCookiePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
