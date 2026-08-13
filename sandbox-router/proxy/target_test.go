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

import "testing"

func TestUpstreamURL(t *testing.T) {
	cases := []struct {
		name          string
		target        Target
		scheme        string
		clusterDomain string
		path          string
		rawQuery      string
		want          string
	}{
		{
			name:          "dns form default cluster domain",
			target:        Target{ID: "test-box", Namespace: "prod", Port: 9999},
			clusterDomain: "cluster.local",
			path:          "/some/path",
			want:          "http://test-box.prod.svc.cluster.local:9999/some/path",
		},
		{
			name:          "pod ip skips dns",
			target:        Target{ID: "test-box", Namespace: "prod", Port: 9999, PodIP: "10.20.30.40"},
			clusterDomain: "cluster.local",
			path:          "/some/path",
			want:          "http://10.20.30.40:9999/some/path",
		},
		{
			name:          "custom cluster domain",
			target:        Target{ID: "x", Namespace: "ns", Port: 8888},
			clusterDomain: "my.zone",
			path:          "/",
			want:          "http://x.ns.svc.my.zone:8888/",
		},
		{
			name:          "query string preserved",
			target:        Target{ID: "x", Namespace: "ns", Port: 8888},
			clusterDomain: "cluster.local",
			path:          "/api",
			rawQuery:      "a=1&b=2",
			want:          "http://x.ns.svc.cluster.local:8888/api?a=1&b=2",
		},
		{
			name:          "scheme defaults to http",
			target:        Target{ID: "x", Namespace: "ns", Port: 8888},
			scheme:        "",
			clusterDomain: "cluster.local",
			path:          "/",
			want:          "http://x.ns.svc.cluster.local:8888/",
		},
		{
			name:          "explicit scheme honored",
			target:        Target{ID: "x", Namespace: "ns", Port: 8888},
			scheme:        "https",
			clusterDomain: "cluster.local",
			path:          "/",
			want:          "https://x.ns.svc.cluster.local:8888/",
		},
		{
			// Pod IPs on dual-stack / IPv6-only clusters arrive as bare
			// IPv6 strings from Pod.Status.PodIP — must be bracketed in
			// the URL so net/http parses host:port correctly. Without
			// brackets "::1:8888" is ambiguous and the request fails
			// before it leaves the router.
			name:          "ipv6 pod ip bracketed",
			target:        Target{ID: "x", Namespace: "ns", Port: 8888, PodIP: "2001:db8::1"},
			clusterDomain: "cluster.local",
			path:          "/api",
			want:          "http://[2001:db8::1]:8888/api",
		},
		{
			name:          "ipv6 loopback pod ip bracketed",
			target:        Target{ID: "x", Namespace: "ns", Port: 9000, PodIP: "::1"},
			clusterDomain: "cluster.local",
			path:          "/",
			want:          "http://[::1]:9000/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.target.UpstreamURL(tc.scheme, tc.clusterDomain, tc.path, tc.rawQuery).String()
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
