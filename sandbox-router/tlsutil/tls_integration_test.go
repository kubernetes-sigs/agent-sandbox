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

//go:build integration

package tlsutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// startTLSServer brings up an http.Server listening on a random port using
// tc. It returns the URL plus a stop func.
func startTLSServer(t *testing.T, tc *tls.Config) (url string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ln)
	}()

	stop = func() {
		_ = srv.Shutdown(context.Background())
		wg.Wait()
	}
	return "https://" + ln.Addr().String() + "/", stop
}

// trustPool returns a CertPool containing the certificate from c. Used so
// the test client trusts our self-signed server cert.
func trustPool(t *testing.T, c generatedCert) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.CertPEM) {
		t.Fatalf("appendCertsFromPEM failed")
	}
	return pool
}

// clientWithCert builds an *http.Client trusting serverCert. If clientCert is
// non-nil it is presented during handshake.
func clientWithCert(t *testing.T, serverCert generatedCert, clientCert *generatedCert) *http.Client {
	t.Helper()
	tc := &tls.Config{
		RootCAs:    trustPool(t, serverCert),
		ServerName: "localhost",
	}
	if clientCert != nil {
		pair, err := tls.X509KeyPair(clientCert.CertPEM, clientCert.KeyPEM)
		if err != nil {
			t.Fatalf("X509KeyPair: %v", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tc,
			DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
	}
}

func TestIntegration_TLSWithoutMTLS(t *testing.T) {
	serverCert := genSelfSignedCert(t, "server")
	srvCertPath, srvKeyPath := writeCert(t, serverCert)
	reloader, err := NewCertReloader(srvCertPath, srvKeyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("reloader: %v", err)
	}
	cfg := &config.Config{MTLSMode: config.MTLSOff}
	tc, err := BuildServerTLS(cfg, reloader)
	if err != nil {
		t.Fatalf("BuildServerTLS: %v", err)
	}
	url, stop := startTLSServer(t, tc)
	defer stop()

	resp, err := clientWithCert(t, serverCert, nil).Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
}

func TestIntegration_MTLSOptional(t *testing.T) {
	serverCert := genSelfSignedCert(t, "server")
	clientCert := genSelfSignedCert(t, "client")
	caPath := writeCABundle(t, clientCert.CertPEM)
	srvCertPath, srvKeyPath := writeCert(t, serverCert)
	reloader, err := NewCertReloader(srvCertPath, srvKeyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("reloader: %v", err)
	}
	cfg := &config.Config{MTLSMode: config.MTLSOptional, TLSClientCAFile: caPath}
	tc, err := BuildServerTLS(cfg, reloader)
	if err != nil {
		t.Fatalf("BuildServerTLS: %v", err)
	}
	url, stop := startTLSServer(t, tc)
	defer stop()

	t.Run("without client cert allowed", func(t *testing.T) {
		resp, err := clientWithCert(t, serverCert, nil).Get(url)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: got %d want 200", resp.StatusCode)
		}
	})

	t.Run("with valid client cert allowed", func(t *testing.T) {
		resp, err := clientWithCert(t, serverCert, &clientCert).Get(url)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: got %d want 200", resp.StatusCode)
		}
	})
}

func TestIntegration_MTLSRequired(t *testing.T) {
	serverCert := genSelfSignedCert(t, "server")
	clientCert := genSelfSignedCert(t, "client")
	rogueCert := genSelfSignedCert(t, "rogue")
	caPath := writeCABundle(t, clientCert.CertPEM)
	srvCertPath, srvKeyPath := writeCert(t, serverCert)
	reloader, err := NewCertReloader(srvCertPath, srvKeyPath, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("reloader: %v", err)
	}
	cfg := &config.Config{MTLSMode: config.MTLSRequired, TLSClientCAFile: caPath}
	tc, err := BuildServerTLS(cfg, reloader)
	if err != nil {
		t.Fatalf("BuildServerTLS: %v", err)
	}
	url, stop := startTLSServer(t, tc)
	defer stop()

	t.Run("without client cert rejected", func(t *testing.T) {
		_, err := clientWithCert(t, serverCert, nil).Get(url)
		if err == nil {
			t.Fatalf("expected TLS handshake error, got nil")
		}
		// Accept either "tls" or "remote error" / "certificate required" substrings.
		if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("expected tls/certificate error, got: %v", err)
		}
	})

	t.Run("with rogue cert rejected", func(t *testing.T) {
		_, err := clientWithCert(t, serverCert, &rogueCert).Get(url)
		if err == nil {
			t.Fatalf("expected handshake rejection for untrusted client cert")
		}
	})

	t.Run("with trusted cert allowed", func(t *testing.T) {
		resp, err := clientWithCert(t, serverCert, &clientCert).Get(url)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: got %d want 200", resp.StatusCode)
		}
	})
}
