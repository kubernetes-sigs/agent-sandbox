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
	"strings"
	"testing"
)

func hdr(kv map[string]string) http.Header {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return h
}

func TestParseSandboxHeaders(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		want     Target
		wantCode int // 0 means success
	}{
		{
			name:    "happy path",
			headers: map[string]string{HeaderSandboxID: "my-box", HeaderSandboxNamespace: "prod", HeaderSandboxPort: "9000"},
			want:    Target{ID: "my-box", Namespace: "prod", Port: 9000},
		},
		{
			name:    "defaults namespace and port",
			headers: map[string]string{HeaderSandboxID: "my-box"},
			want:    Target{ID: "my-box", Namespace: DefaultSandboxNamespace, Port: DefaultSandboxPort},
		},
		{
			name:    "pod-ip overrides DNS path",
			headers: map[string]string{HeaderSandboxID: "my-box", HeaderSandboxPodIP: "10.0.0.5"},
			want:    Target{ID: "my-box", Namespace: DefaultSandboxNamespace, Port: DefaultSandboxPort, PodIP: "10.0.0.5"},
		},
		{
			name:    "uid header captured",
			headers: map[string]string{HeaderSandboxID: "my-box", HeaderSandboxUID: "abc-123-uid"},
			want:    Target{ID: "my-box", UID: "abc-123-uid", Namespace: DefaultSandboxNamespace, Port: DefaultSandboxPort},
		},
		{
			name:    "hyphenated namespace accepted",
			headers: map[string]string{HeaderSandboxID: "my-box", HeaderSandboxNamespace: "my-ns-1"},
			want:    Target{ID: "my-box", Namespace: "my-ns-1", Port: DefaultSandboxPort},
		},
		{
			name:     "missing sandbox id rejected",
			headers:  map[string]string{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "namespace with space rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxNamespace: "bad namespace"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "namespace with bang rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxNamespace: "bad!"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "hyphens-only namespace rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxNamespace: "---"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "non-numeric port rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "abc"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "zero port rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "0"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "negative port rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "-1"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "port above 65535 rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "65536"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:    "port 1 accepted",
			headers: map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "1"},
			want:    Target{ID: "x", Namespace: DefaultSandboxNamespace, Port: 1},
		},
		{
			name:    "port 65535 accepted",
			headers: map[string]string{HeaderSandboxID: "x", HeaderSandboxPort: "65535"},
			want:    Target{ID: "x", Namespace: DefaultSandboxNamespace, Port: 65535},
		},

		// ---- DNS-label validation on ID (matches Python's _is_valid_dns_label) ----
		// These are the inputs the Python router blocks specifically
		// "to prevent DNS injection and directory traversal style
		// attacks". Without these checks an attacker can interpolate
		// arbitrary DNS components into the upstream FQDN.
		{
			name:     "id with dot rejected (would inject extra DNS components)",
			headers:  map[string]string{HeaderSandboxID: "foo.evil.com"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "id with slash rejected (directory traversal flavor)",
			headers:  map[string]string{HeaderSandboxID: "foo/bar"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "id with underscore rejected (not a DNS label char)",
			headers:  map[string]string{HeaderSandboxID: "foo_bar"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "id with uppercase rejected (DNS-1123 is lowercase)",
			headers:  map[string]string{HeaderSandboxID: "FooBar"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "id with leading hyphen rejected",
			headers:  map[string]string{HeaderSandboxID: "-foo"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "id with trailing hyphen rejected",
			headers:  map[string]string{HeaderSandboxID: "foo-"},
			wantCode: http.StatusBadRequest,
		},

		// ---- Pod-IP class check (SSRF defense) ----
		// Without these, an unauthenticated caller (default AllowAll
		// authorizer) can use X-Sandbox-Pod-IP to dial arbitrary
		// internal addresses — cloud metadata, the router's loopback,
		// link-local services, etc. Matches the Python router's
		// ipaddress.ip_address class check.
		{
			name:     "pod-ip loopback rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "127.0.0.1"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip ipv6 loopback rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "::1"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip aws/gcp metadata link-local rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "169.254.169.254"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip ipv6 link-local rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "fe80::1"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip multicast rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "224.0.0.1"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip unspecified rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "0.0.0.0"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip ipv6 unspecified rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "::"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "pod-ip garbage rejected",
			headers:  map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "not-an-ip"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:    "pod-ip routable accepted",
			headers: map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "10.0.0.5"},
			want:    Target{ID: "x", Namespace: DefaultSandboxNamespace, Port: DefaultSandboxPort, PodIP: "10.0.0.5"},
		},
		{
			name:    "pod-ip ipv6 routable accepted",
			headers: map[string]string{HeaderSandboxID: "x", HeaderSandboxPodIP: "2001:db8::1"},
			want:    Target{ID: "x", Namespace: DefaultSandboxNamespace, Port: DefaultSandboxPort, PodIP: "2001:db8::1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, perr := ParseSandboxHeaders(hdr(tc.headers), ParseOptions{})
			if tc.wantCode != 0 {
				if perr == nil {
					t.Fatalf("expected error, got Target=%+v", got)
				}
				if perr.Status != tc.wantCode {
					t.Fatalf("status: got %d, want %d (detail=%q)", perr.Status, tc.wantCode, perr.Detail)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if got != tc.want {
				t.Fatalf("target: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidDNSLabel(t *testing.T) {
	// Note: stricter than the old validNamespace. Uppercase, leading
	// or trailing hyphens, and labels > 63 chars are rejected — same
	// shape as RFC 1123 / K8s' own DNS-label rules and the Python
	// router's _is_valid_dns_label.
	cases := map[string]bool{
		// accepted
		"default":               true,
		"prod":                  true,
		"my-ns":                 true,
		"my-ns-1":               true,
		"a":                     true,
		"abc123":                true,
		"a1":                    true,
		"1abc":                  true, // RFC 1123 (vs the older 952) permits leading digit
		strings.Repeat("a", 63): true,

		// rejected — character class
		"MY-NS":   false, // uppercase
		"my_ns":   false, // underscore
		"my.ns":   false, // dot would inject an extra DNS component
		"foo/bar": false, // slash, traversal flavor
		" ns":     false, // leading space
		"ns ":     false, // trailing space
		"bad!":    false,
		"emoji-🦄": false,

		// rejected — structure
		"":                      false, // empty
		"-":                     false, // single hyphen
		"---":                   false, // hyphens only
		"-x":                    false, // leading hyphen
		"x-":                    false, // trailing hyphen
		strings.Repeat("a", 64): false, // exceeds 63-char cap
	}
	for in, want := range cases {
		if got := validDNSLabel(in); got != want {
			t.Errorf("validDNSLabel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidPodIP(t *testing.T) {
	// wantStrict is the default-deployment verdict (allowLoopback=false).
	// wantLoopback is the sidecar/localhost-test verdict
	// (allowLoopback=true) — same as strict, except loopback v4/v6 flip
	// to accepted. Link-local, multicast, and unspecified stay rejected
	// regardless of the flag.
	type verdicts struct {
		strict   bool
		loopback bool
	}
	cases := map[string]verdicts{
		// loopback — only the loopback flag flips these
		"127.0.0.1":    {strict: false, loopback: true},
		"127.10.20.30": {strict: false, loopback: true}, // anywhere in 127.0.0.0/8
		"::1":          {strict: false, loopback: true},

		// other rejected classes — same verdict regardless of flag
		"0.0.0.0":         {false, false}, // unspecified v4
		"::":              {false, false}, // unspecified v6
		"169.254.169.254": {false, false}, // AWS / GCP metadata service
		"169.254.1.1":     {false, false}, // 169.254.0.0/16 in general
		"fe80::1":         {false, false}, // ipv6 link-local
		"224.0.0.1":       {false, false}, // ipv4 multicast (224.0.0.0/4)
		"239.255.255.250": {false, false}, // SSDP / mDNS-ish
		"ff02::1":         {false, false}, // ipv6 multicast

		// rejected — not a valid IP literal
		"":            {false, false},
		"not-an-ip":   {false, false},
		"10.0.0.256":  {false, false},
		"2001:db8::g": {false, false},
		"10.0.0.1:80": {false, false}, // host:port, not a bare IP
		"[::1]":       {false, false}, // bracketed form, not a bare IP

		// accepted — routable v4 and v6 (flag-independent)
		"10.0.0.5":     {true, true},
		"192.168.1.1":  {true, true}, // private space is fine — caller knows what they're dialing
		"8.8.8.8":      {true, true}, // public IP (class check is the parity line; broader SSRF defense lives in the Authorizer)
		"2001:db8::1":  {true, true},
		"2607:f8b0::1": {true, true},
	}
	for in, want := range cases {
		if got := validPodIP(in, false); got != want.strict {
			t.Errorf("validPodIP(%q, allowLoopback=false) = %v, want %v", in, got, want.strict)
		}
		if got := validPodIP(in, true); got != want.loopback {
			t.Errorf("validPodIP(%q, allowLoopback=true) = %v, want %v", in, got, want.loopback)
		}
	}
}
