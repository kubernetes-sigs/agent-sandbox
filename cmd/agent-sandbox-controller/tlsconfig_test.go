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

package main

import (
	"crypto/tls"
	"testing"
)

func TestBuildMetricsTLSOpts(t *testing.T) {
	tests := []struct {
		name         string
		minVersion   string
		cipherSuites string
		wantNil      bool
		wantErr      bool
	}{
		{"empty_inputs", "", "", true, false},
		{"version_only", "VersionTLS12", "", false, false},
		{"ciphers_only", "", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", false, false},
		{"version_and_ciphers", "VersionTLS12", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", false, false},
		{"invalid_version", "VersionTLS99", "", false, true},
		{"invalid_cipher", "VersionTLS12", "BOGUS_CIPHER", false, true},
		{"tls13_skips_ciphers", "VersionTLS13", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildMetricsTLSOpts(tt.minVersion, tt.cipherSuites)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildMetricsTLSOpts(%q, %q) error = %v, wantErr %v", tt.minVersion, tt.cipherSuites, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantNil && len(got) != 0 {
				t.Errorf("expected nil/empty opts, got %d", len(got))
			}
			if !tt.wantNil && len(got) == 0 {
				t.Errorf("expected non-empty opts, got nil")
			}
		})
	}

	t.Run("applies_minversion", func(t *testing.T) {
		opts, err := buildMetricsTLSOpts("VersionTLS13", "")
		if err != nil {
			t.Fatal(err)
		}
		cfg := &tls.Config{}
		for _, fn := range opts {
			fn(cfg)
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("applies_ciphers", func(t *testing.T) {
		opts, err := buildMetricsTLSOpts("VersionTLS12", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256")
		if err != nil {
			t.Fatal(err)
		}
		cfg := &tls.Config{}
		for _, fn := range opts {
			fn(cfg)
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS12)
		}
		if len(cfg.CipherSuites) != 1 || cfg.CipherSuites[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
			t.Errorf("CipherSuites = %v, want [%d]", cfg.CipherSuites, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
		}
	})

	t.Run("tls13_no_ciphers_applied", func(t *testing.T) {
		opts, err := buildMetricsTLSOpts("VersionTLS13", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256")
		if err != nil {
			t.Fatal(err)
		}
		cfg := &tls.Config{}
		for _, fn := range opts {
			fn(cfg)
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS13)
		}
		if len(cfg.CipherSuites) != 0 {
			t.Errorf("CipherSuites should be empty for TLS 1.3, got %v", cfg.CipherSuites)
		}
	})
}
