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

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

func TestBuildServerTLS_TLSProfile(t *testing.T) {
	reloader := newReloaderForTest(t)

	t.Run("default MinVersion is TLS 1.2", func(t *testing.T) {
		cfg := &config.Config{MTLSMode: config.MTLSOff}
		tc, err := BuildServerTLS(cfg, reloader)
		if err != nil {
			t.Fatalf("BuildServerTLS: %v", err)
		}
		if tc.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion: got %d, want %d", tc.MinVersion, tls.VersionTLS12)
		}
	})

	t.Run("custom MinVersion", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode:      config.MTLSOff,
			TLSMinVersion: "VersionTLS13",
		}
		tc, err := BuildServerTLS(cfg, reloader)
		if err != nil {
			t.Fatalf("BuildServerTLS: %v", err)
		}
		if tc.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion: got %d, want %d", tc.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("invalid MinVersion", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode:      config.MTLSOff,
			TLSMinVersion: "bogus",
		}
		_, err := BuildServerTLS(cfg, reloader)
		if err == nil {
			t.Fatalf("expected error for invalid min version")
		}
	})

	t.Run("cipher suites applied", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode: config.MTLSOff,
			TLSCipherSuites: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			},
		}
		tc, err := BuildServerTLS(cfg, reloader)
		if err != nil {
			t.Fatalf("BuildServerTLS: %v", err)
		}
		if len(tc.CipherSuites) != 2 {
			t.Fatalf("CipherSuites: got %d entries, want 2", len(tc.CipherSuites))
		}
	})

	t.Run("cipher suites ignored for TLS 1.3", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode:      config.MTLSOff,
			TLSMinVersion: "VersionTLS13",
			TLSCipherSuites: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
		}
		tc, err := BuildServerTLS(cfg, reloader)
		if err != nil {
			t.Fatalf("BuildServerTLS: %v", err)
		}
		if len(tc.CipherSuites) != 0 {
			t.Errorf("CipherSuites should be empty for TLS 1.3, got %d", len(tc.CipherSuites))
		}
	})

	t.Run("invalid cipher suite", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode:        config.MTLSOff,
			TLSCipherSuites: []string{"TLS_BOGUS"},
		}
		_, err := BuildServerTLS(cfg, reloader)
		if err == nil {
			t.Fatalf("expected error for invalid cipher suite")
		}
	})

	t.Run("invalid cipher suite rejected even with TLS 1.3", func(t *testing.T) {
		cfg := &config.Config{
			MTLSMode:        config.MTLSOff,
			TLSMinVersion:   "VersionTLS13",
			TLSCipherSuites: []string{"TLS_BOGUS"},
		}
		_, err := BuildServerTLS(cfg, reloader)
		if err == nil {
			t.Fatalf("expected error for invalid cipher suite even with TLS 1.3")
		}
	})
}
