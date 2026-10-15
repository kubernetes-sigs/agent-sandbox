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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// newReloaderForTest returns a CertReloader backed by a fresh self-signed
// cert; used to satisfy BuildServerTLS's non-nil reloader requirement.
func newReloaderForTest(t *testing.T) *CertReloader {
	t.Helper()
	c := genSelfSignedCert(t, "leaf")
	certPath, keyPath := writeCert(t, c)
	r, err := NewCertReloader(certPath, keyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	return r
}

func writeCABundle(t *testing.T, pem []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(p, pem, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return p
}

func TestBuildServerTLS_MTLSModeMapping(t *testing.T) {
	caCert := genSelfSignedCert(t, "ca").CertPEM
	caPath := writeCABundle(t, caCert)
	reloader := newReloaderForTest(t)

	cases := []struct {
		mode   config.MTLSMode
		want   tls.ClientAuthType
		needCA bool
	}{
		{config.MTLSOff, tls.NoClientCert, false},
		{config.MTLSOptional, tls.VerifyClientCertIfGiven, true},
		{config.MTLSRequired, tls.RequireAndVerifyClientCert, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := &config.Config{MTLSMode: tc.mode}
			if tc.needCA {
				cfg.TLSClientCAFile = caPath
			}
			tc2, err := BuildServerTLS(cfg, reloader)
			if err != nil {
				t.Fatalf("BuildServerTLS: %v", err)
			}
			if tc2.ClientAuth != tc.want {
				t.Errorf("ClientAuth: got %v want %v", tc2.ClientAuth, tc.want)
			}
			if tc.needCA && tc2.ClientCAs == nil {
				t.Errorf("ClientCAs should be set when mode != off")
			}
			if !tc.needCA && tc2.ClientCAs != nil {
				t.Errorf("ClientCAs should be nil when mode == off")
			}
			if tc2.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion should be TLS 1.2")
			}
			if tc2.GetCertificate == nil {
				t.Errorf("GetCertificate must be set")
			}
		})
	}
}

func TestBuildServerTLS_NilReloader(t *testing.T) {
	_, err := BuildServerTLS(&config.Config{MTLSMode: config.MTLSOff}, nil)
	if err == nil {
		t.Fatalf("expected error for nil reloader")
	}
}

func TestBuildServerTLS_InvalidMode(t *testing.T) {
	_, err := BuildServerTLS(&config.Config{MTLSMode: "bogus"}, newReloaderForTest(t))
	if err == nil {
		t.Fatalf("expected error for invalid mode")
	}
}

func TestUpstreamTLSClientConfig(t *testing.T) {
	caPath := writeCABundle(t, genSelfSignedCert(t, "ca").CertPEM)
	ca, err := NewCAReloader(caPath, logr.Discard())
	if err != nil {
		t.Fatalf("NewCAReloader: %v", err)
	}
	u := BuildUpstreamTLS(ca, newReloaderForTest(t))

	a := u.ClientConfig("sb-a.test.svc.cluster.local")
	b := u.ClientConfig("sb-b.test.svc.cluster.local")
	if a == b {
		t.Fatalf("connections share one tls.Config")
	}
	if a.ServerName != "sb-a.test.svc.cluster.local" || b.ServerName != "sb-b.test.svc.cluster.local" {
		t.Fatalf("ServerName = %q, %q", a.ServerName, b.ServerName)
	}
	if a.MinVersion != tls.VersionTLS12 || a.CipherSuites != nil {
		t.Fatalf("MinVersion = %#x, CipherSuites = %v; want TLS 1.2 and Go defaults", a.MinVersion, a.CipherSuites)
	}
	if a.RootCAs != ca.Pool() {
		t.Fatalf("RootCAs is not the CA bundle")
	}
	if a.GetClientCertificate == nil {
		t.Fatalf("no client certificate")
	}
	// crypto/tls must do all of the verification, so FIPS 140-3 mode
	// applies its checks too.
	if a.InsecureSkipVerify || a.VerifyConnection != nil || a.VerifyPeerCertificate != nil {
		t.Fatalf("verification is replaced or skipped")
	}

	// A connection opened after the bundle is reloaded uses the new bundle.
	before := ca.Pool()
	if err := os.WriteFile(caPath, genSelfSignedCert(t, "ca2").CertPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := ca.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := u.ClientConfig("sb-a.test.svc.cluster.local").RootCAs; got == before || got != ca.Pool() {
		t.Fatalf("RootCAs did not follow the reload")
	}
}

func TestUpstreamTLSClientConfigSystemRoots(t *testing.T) {
	c := BuildUpstreamTLS(nil, nil).ClientConfig("sb-a.test.svc.cluster.local")
	if c.RootCAs != nil {
		t.Fatalf("RootCAs set; want nil for the system roots")
	}
	if c.GetClientCertificate != nil {
		t.Fatalf("client certificate set without one configured")
	}
}

func TestLoadCAPool(t *testing.T) {
	t.Run("valid cert", func(t *testing.T) {
		c := genSelfSignedCert(t, "ca")
		path := writeCABundle(t, c.CertPEM)
		pool, err := LoadCAPool(path)
		if err != nil {
			t.Fatalf("LoadCAPool: %v", err)
		}
		if pool == nil {
			t.Fatalf("pool nil")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := LoadCAPool("/nope")
		if err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		path := writeCABundle(t, nil)
		_, err := LoadCAPool(path)
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("expected empty-file error, got %v", err)
		}
	})
	t.Run("non-PEM file", func(t *testing.T) {
		path := writeCABundle(t, []byte("not a certificate"))
		_, err := LoadCAPool(path)
		if err == nil || !strings.Contains(err.Error(), "no parseable") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})
}
