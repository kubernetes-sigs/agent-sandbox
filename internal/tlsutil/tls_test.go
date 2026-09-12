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

package tlsutil

import (
	"crypto/tls"
	"testing"
)

func TestParseTLSVersion(t *testing.T) {
	cases := []struct {
		input string
		want  uint16
		err   bool
	}{
		{"VersionTLS10", tls.VersionTLS10, false},
		{"VersionTLS11", tls.VersionTLS11, false},
		{"VersionTLS12", tls.VersionTLS12, false},
		{"VersionTLS13", tls.VersionTLS13, false},
		{"TLS1.2", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		name := tc.input
		if name == "" {
			name = "<empty>"
		}
		t.Run(name, func(t *testing.T) {
			got, err := ParseTLSVersion(tc.input)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseCipherSuites(t *testing.T) {
	t.Run("nil on empty", func(t *testing.T) {
		ids, err := ParseCipherSuites(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ids != nil {
			t.Fatalf("expected nil, got %v", ids)
		}
	})

	t.Run("valid suites", func(t *testing.T) {
		names := []string{
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		}
		ids, err := ParseCipherSuites(names)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 2 {
			t.Fatalf("expected 2 IDs, got %d", len(ids))
		}
		if ids[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
			t.Errorf("first suite: got %#x, want %#x", ids[0], tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
		}
		if ids[1] != tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384 {
			t.Errorf("second suite: got %#x, want %#x", ids[1], tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384)
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		ids, err := ParseCipherSuites([]string{"  TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("expected 1 ID, got %d", len(ids))
		}
	})

	t.Run("skips empty entries", func(t *testing.T) {
		ids, err := ParseCipherSuites([]string{"", "  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("expected 0 IDs, got %d", len(ids))
		}
	})

	t.Run("unknown suite", func(t *testing.T) {
		_, err := ParseCipherSuites([]string{"TLS_BOGUS_CIPHER"})
		if err == nil {
			t.Fatalf("expected error for unknown suite")
		}
	})

	t.Run("silently skips TLS 1.3 suites", func(t *testing.T) {
		names := []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
		}
		ids, err := ParseCipherSuites(names)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("expected 0 IDs (all TLS 1.3 skipped), got %d", len(ids))
		}
	})

	t.Run("mixed TLS 1.2 and 1.3 suites", func(t *testing.T) {
		names := []string{
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
		}
		ids, err := ParseCipherSuites(names)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("expected 1 ID (TLS 1.3 skipped), got %d", len(ids))
		}
		if ids[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
			t.Errorf("got %#x, want %#x", ids[0], tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
		}
	})

	t.Run("rejects insecure suites", func(t *testing.T) {
		for _, suite := range tls.InsecureCipherSuites() {
			t.Run(suite.Name, func(t *testing.T) {
				_, err := ParseCipherSuites([]string{suite.Name})
				if err == nil {
					t.Errorf("expected error for insecure suite %q", suite.Name)
				}
			})
		}
	})
}

func TestIsTLS13CipherSuite(t *testing.T) {
	tls13 := []string{
		"TLS_AES_128_GCM_SHA256",
		"TLS_AES_256_GCM_SHA384",
		"TLS_CHACHA20_POLY1305_SHA256",
	}
	for _, name := range tls13 {
		if !IsTLS13CipherSuite(name) {
			t.Errorf("expected %q to be a TLS 1.3 suite", name)
		}
	}

	notTLS13 := []string{
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"TLS_BOGUS",
		"",
	}
	for _, name := range notTLS13 {
		if IsTLS13CipherSuite(name) {
			t.Errorf("expected %q to NOT be a TLS 1.3 suite", name)
		}
	}
}
