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
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
)

// fakeLookup is a minimal Lookup implementation for tests. It records
// Invalidate calls so the proxy ErrorHandler tests can assert on them.
type fakeLookup struct {
	entries     map[types.UID]cache.Entry
	invalidated []types.UID
}

func (f *fakeLookup) Get(uid types.UID) (cache.Entry, bool) {
	e, ok := f.entries[uid]
	return e, ok
}

func (f *fakeLookup) Invalidate(uid types.UID) bool {
	_, ok := f.entries[uid]
	if ok {
		delete(f.entries, uid)
	}
	f.invalidated = append(f.invalidated, uid)
	return ok
}

func TestResolve(t *testing.T) {
	const cd = "cluster.local"
	cases := []struct {
		name       string
		target     Target
		lookup     Lookup
		wantURL    string
		wantSource Source
	}{
		{
			name:       "pod ip override beats everything",
			target:     Target{ID: "id", UID: "u1", Namespace: "ns", Port: 9999, PodIP: "10.0.0.1"},
			lookup:     &fakeLookup{entries: map[types.UID]cache.Entry{"u1": {PodIP: "10.0.0.99"}}},
			wantURL:    "http://10.0.0.1:9999",
			wantSource: SourcePodIP,
		},
		{
			name:       "cache hit by UID",
			target:     Target{ID: "id", UID: "u1", Namespace: "ns", Port: 9999},
			lookup:     &fakeLookup{entries: map[types.UID]cache.Entry{"u1": {PodIP: "10.0.0.42"}}},
			wantURL:    "http://10.0.0.42:9999",
			wantSource: SourceCache,
		},
		{
			name:       "cache miss falls back to DNS",
			target:     Target{ID: "id", UID: "u1", Namespace: "ns", Port: 9999},
			lookup:     &fakeLookup{entries: map[types.UID]cache.Entry{}},
			wantURL:    "http://id.ns.svc.cluster.local:9999",
			wantSource: SourceDNS,
		},
		{
			name:       "no UID supplied, lookup non-nil",
			target:     Target{ID: "id", Namespace: "ns", Port: 9999},
			lookup:     &fakeLookup{entries: map[types.UID]cache.Entry{"u1": {PodIP: "10.0.0.42"}}},
			wantURL:    "http://id.ns.svc.cluster.local:9999",
			wantSource: SourceDNS,
		},
		{
			name:       "nil lookup falls through to DNS",
			target:     Target{ID: "id", UID: "u1", Namespace: "ns", Port: 9999},
			lookup:     nil,
			wantURL:    "http://id.ns.svc.cluster.local:9999",
			wantSource: SourceDNS,
		},
		{
			name:       "scheme defaults to http",
			target:     Target{ID: "id", Namespace: "ns", Port: 9999},
			wantURL:    "http://id.ns.svc.cluster.local:9999",
			wantSource: SourceDNS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotSrc := tc.target.Resolve("", cd, "", "", tc.lookup)
			if gotURL.String() != tc.wantURL {
				t.Fatalf("url: got %q want %q", gotURL.String(), tc.wantURL)
			}
			if gotSrc != tc.wantSource {
				t.Fatalf("source: got %q want %q", gotSrc, tc.wantSource)
			}
		})
	}
}

func TestResolvePreservesPathAndQuery(t *testing.T) {
	tgt := Target{ID: "id", Namespace: "ns", Port: 8888}
	got, _ := tgt.Resolve("https", "cluster.local", "/api/v1/things", "a=1&b=2", nil)
	want := "https://id.ns.svc.cluster.local:8888/api/v1/things?a=1&b=2"
	if got.String() != want {
		t.Fatalf("got %q want %q", got.String(), want)
	}
}

func TestResolveBracketsIPv6PodIP(t *testing.T) {
	// Cache returns a bare IPv6 string (matches what Pod.Status.PodIP
	// looks like on dual-stack / IPv6-only clusters). The resolved URL
	// must bracket the literal so net/http can parse it.
	lookup := &fakeLookup{entries: map[types.UID]cache.Entry{
		"v6": {PodIP: "2001:db8::42"},
	}}
	tgt := Target{ID: "id", UID: "v6", Namespace: "ns", Port: 8888}
	got, src := tgt.Resolve("http", "cluster.local", "/api", "", lookup)
	if got.String() != "http://[2001:db8::42]:8888/api" {
		t.Fatalf("ipv6 cache hit: got %q want http://[2001:db8::42]:8888/api", got.String())
	}
	if src != SourceCache {
		t.Fatalf("source: got %q want cache", src)
	}

	// Same expectation for an explicit X-Sandbox-Pod-IP override.
	tgt = Target{ID: "id", Namespace: "ns", Port: 8888, PodIP: "fe80::1"}
	got, src = tgt.Resolve("http", "cluster.local", "/", "", nil)
	if got.String() != "http://[fe80::1]:8888/" {
		t.Fatalf("ipv6 override: got %q want http://[fe80::1]:8888/", got.String())
	}
	if src != SourcePodIP {
		t.Fatalf("source: got %q want pod-ip", src)
	}
}
