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
	"crypto/x509"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// leafSerial extracts the leaf certificate's serial number so tests can
// compare two reloaded certs without reaching into private fields.
func leafSerial(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf.SerialNumber.String()
}

func TestNewCertReloader_LoadsInitial(t *testing.T) {
	c := genSelfSignedCert(t, "leaf-1")
	certPath, keyPath := writeCert(t, c)

	r, err := NewCertReloader(certPath, keyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(got.Certificate) == 0 {
		t.Fatalf("loaded certificate is empty")
	}
}

func TestNewCertReloader_FailsOnBadPath(t *testing.T) {
	_, err := NewCertReloader("/nope/cert.pem", "/nope/key.pem", logr.Discard(), nil)
	if err == nil {
		t.Fatalf("expected error for missing files")
	}
}

func TestNewCertReloader_RejectsEmptyPaths(t *testing.T) {
	_, err := NewCertReloader("", "", logr.Discard(), nil)
	if err == nil {
		t.Fatalf("expected error for empty paths")
	}
}

func TestCertReloader_HotReload(t *testing.T) {
	first := genSelfSignedCert(t, "leaf-1")
	certPath, keyPath := writeCert(t, first)

	var ok atomic.Int32
	cb := func(success bool, _ error) {
		if success {
			ok.Add(1)
		}
	}
	r, err := NewCertReloader(certPath, keyPath, logr.Discard(), cb)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	if ok.Load() != 1 {
		t.Fatalf("expected initial load callback")
	}

	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	beforeCert, _ := r.GetCertificate(nil)
	beforeSerial := leafSerial(t, beforeCert)

	// Replace the cert with a fresh self-signed pair. Write to a tmp file
	// and rename to mimic atomic Secret rotation.
	second := genSelfSignedCert(t, "leaf-2")
	tmpCert := certPath + ".tmp"
	tmpKey := keyPath + ".tmp"
	if err := os.WriteFile(tmpCert, second.CertPEM, 0o600); err != nil {
		t.Fatalf("write tmp cert: %v", err)
	}
	if err := os.WriteFile(tmpKey, second.KeyPEM, 0o600); err != nil {
		t.Fatalf("write tmp key: %v", err)
	}
	if err := os.Rename(tmpCert, certPath); err != nil {
		t.Fatalf("rename cert: %v", err)
	}
	if err := os.Rename(tmpKey, keyPath); err != nil {
		t.Fatalf("rename key: %v", err)
	}

	// Wait for the debounced reload to fire and the atomic swap to complete.
	// Two strategies: poll the cert serial, with a generous timeout.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after, err := r.GetCertificate(nil)
		if err == nil {
			if leafSerial(t, after) != beforeSerial {
				return // success
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("certificate was not reloaded within 5s")
}
