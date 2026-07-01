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
	"path/filepath"
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

// TestCertReloader_KubernetesAtomicWriterRotation reproduces the
// projected-Secret rotation pattern used by kubelet's AtomicWriter,
// which is what mounts TLS material into a Pod in production. The
// leaf file paths the reloader watches are themselves symlinks:
//
//	/tls/tls.crt → ..data/tls.crt
//	/tls/tls.key → ..data/tls.key
//	/tls/..data  → ..TIMESTAMP/         (the swap target)
//
// On rotation, kubelet writes the new content to a fresh
// ..NEWTIMESTAMP/ directory and atomically renames ..data to point at
// it. fsnotify on the parent reports CREATE/REMOVE on the
// "..TIMESTAMP" siblings and RENAME on "..data" — the leaf file path
// is never named in any event. The original filter rejected all of
// these, so K8s rotations were silently missed in production. This
// test pins the fixed behavior.
func TestCertReloader_KubernetesAtomicWriterRotation(t *testing.T) {
	first := genSelfSignedCert(t, "leaf-v1")

	dir := t.TempDir()
	dataV1 := filepath.Join(dir, "..2026_01_01_00_00_00.0")
	if err := os.Mkdir(dataV1, 0o700); err != nil {
		t.Fatalf("mkdir v1 data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataV1, "tls.crt"), first.CertPEM, 0o600); err != nil {
		t.Fatalf("write v1 cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataV1, "tls.key"), first.KeyPEM, 0o600); err != nil {
		t.Fatalf("write v1 key: %v", err)
	}
	if err := os.Symlink(filepath.Base(dataV1), filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.Symlink(filepath.Join("..data", "tls.crt"), certPath); err != nil {
		t.Fatalf("symlink leaf cert: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "tls.key"), keyPath); err != nil {
		t.Fatalf("symlink leaf key: %v", err)
	}

	r, err := NewCertReloader(certPath, keyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before, _ := r.GetCertificate(nil)
	beforeSerial := leafSerial(t, before)

	// Stage the new version exactly as kubelet would: fresh
	// "..NEWTIMESTAMP" dir, then atomically rename ..data to point at
	// it. The os.Rename on a symlink IS the atomic swap.
	second := genSelfSignedCert(t, "leaf-v2")
	dataV2 := filepath.Join(dir, "..2026_01_01_00_00_05.0")
	if err := os.Mkdir(dataV2, 0o700); err != nil {
		t.Fatalf("mkdir v2 data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataV2, "tls.crt"), second.CertPEM, 0o600); err != nil {
		t.Fatalf("write v2 cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataV2, "tls.key"), second.KeyPEM, 0o600); err != nil {
		t.Fatalf("write v2 key: %v", err)
	}
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(dataV2), tmpLink); err != nil {
		t.Fatalf("stage ..data_tmp: %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("atomic ..data swap: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after, err := r.GetCertificate(nil)
		if err == nil && leafSerial(t, after) != beforeSerial {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("certificate was not reloaded after Kubernetes-style ..data symlink swap")
}
