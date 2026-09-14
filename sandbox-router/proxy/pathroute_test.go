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
