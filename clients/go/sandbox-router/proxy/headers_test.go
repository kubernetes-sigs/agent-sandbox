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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, perr := ParseSandboxHeaders(hdr(tc.headers))
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

func TestValidNamespace(t *testing.T) {
	cases := map[string]bool{
		"default": true,
		"prod":    true,
		"my-ns":   true,
		"my-ns-1": true,
		"MY-NS":   true,
		"a":       true,
		"":        false,
		"-":       false,
		"---":     false,
		"my_ns":   false,
		"my.ns":   false,
		" ns":     false,
		"ns ":     false,
		"bad!":    false,
		"emoji-🦄": false,
	}
	for in, want := range cases {
		if got := validNamespace(in); got != want {
			t.Errorf("validNamespace(%q) = %v, want %v", in, got, want)
		}
	}
}
