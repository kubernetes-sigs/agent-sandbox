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
		name         string
		prefix       string
		path         string
		wantMatched  bool
		wantCode     int // 0 means success (or "not matched", see wantMatched)
		wantTarget   Target
		wantUpstream string
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
			name:         "happy path, no trailing content",
			prefix:       "/router",
			path:         "/router/test/my-box/8080",
			wantMatched:  true,
			wantTarget:   Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream: "/",
		},
		{
			name:         "happy path, trailing slash only",
			prefix:       "/router",
			path:         "/router/test/my-box/8080/",
			wantMatched:  true,
			wantTarget:   Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream: "/",
		},
		{
			name:         "happy path, nested remainder preserved verbatim",
			prefix:       "/router",
			path:         "/router/test/my-box/8080/stable-abc/static/out/vs/code.js",
			wantMatched:  true,
			wantTarget:   Target{ID: "my-box", Namespace: "test", Port: 8080},
			wantUpstream: "/stable-abc/static/out/vs/code.js",
		},
		{
			name:        "empty prefix means root-mounted routing",
			prefix:      "",
			path:        "",
			wantMatched: false, // prefix=="" is always "disabled", see first case
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
			name:         "port 1 accepted",
			prefix:       "/router",
			path:         "/router/test/my-box/1/",
			wantMatched:  true,
			wantTarget:   Target{ID: "my-box", Namespace: "test", Port: 1},
			wantUpstream: "/",
		},
		{
			name:         "port 65535 accepted",
			prefix:       "/router",
			path:         "/router/test/my-box/65535/",
			wantMatched:  true,
			wantTarget:   Target{ID: "my-box", Namespace: "test", Port: 65535},
			wantUpstream: "/",
		},
		{
			name:         "root-mounted prefix",
			prefix:       "/sandboxes",
			path:         "/sandboxes/poc-agent-sandbox/sandbox-abc123/4200/",
			wantMatched:  true,
			wantTarget:   Target{ID: "sandbox-abc123", Namespace: "poc-agent-sandbox", Port: 4200},
			wantUpstream: "/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, upstream, matched, perr := ParsePathRoute(tc.prefix, tc.path)
			if matched != tc.wantMatched {
				t.Fatalf("matched: got %v, want %v (target=%+v, upstream=%q, perr=%v)",
					matched, tc.wantMatched, target, upstream, perr)
			}
			if !matched {
				return // nothing else to check — caller falls through to headers
			}
			if tc.wantCode != 0 {
				if perr == nil {
					t.Fatalf("expected error, got target=%+v upstream=%q", target, upstream)
				}
				if perr.Status != tc.wantCode {
					t.Fatalf("status: got %d, want %d (detail=%q)", perr.Status, tc.wantCode, perr.Detail)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if target != tc.wantTarget {
				t.Fatalf("target: got %+v, want %+v", target, tc.wantTarget)
			}
			if upstream != tc.wantUpstream {
				t.Fatalf("upstreamPath: got %q, want %q", upstream, tc.wantUpstream)
			}
		})
	}
}
