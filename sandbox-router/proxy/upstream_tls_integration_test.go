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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
	"sigs.k8s.io/agent-sandbox/sandbox-router/tlsutil"
)

// testCA signs leaf certificates for the upstream TLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate signed by ca and its private key, both
// PEM-encoded. The certificate lists dnsNames as its DNS subject
// alternative names, and usage sets its extended key usage (server or
// client authentication).
func (ca *testCA) issue(t *testing.T, cn string, dnsNames []string, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("gen serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// newTLSBackend starts an HTTPS backend serving a certificate for sandbox
// "sb-a" in namespace "test", named sb-a.test.svc.<domain>, and requiring a
// client certificate signed by ca. It records the client certificate CN and
// Host it saw.
func newTLSBackend(t *testing.T, ca *testCA, domain string) (backend *httptest.Server, seen func() (cn, host string)) {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, "sb-a", []string{"sb-a.test.svc." + domain}, x509.ExtKeyUsageServerAuth)
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load backend cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	var (
		mu               sync.Mutex
		seenCN, seenHost string
	)
	backend = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenCN = r.TLS.PeerCertificates[0].Subject.CommonName
		seenHost = r.Host
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	backend.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	return backend, func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return seenCN, seenHost
	}
}

// upstreamRouterOpts selects how newUpstreamTLSRouter configures upstream TLS.
type upstreamRouterOpts struct {
	// If clientCert is set, the router presents a client certificate
	// signed by ca, with CN "sandbox-router".
	clientCert bool
	// If systemRoots is set, the router runs without
	// --upstream-tls-ca-file, so it verifies sandbox certificates against
	// the system roots.
	systemRoots bool
	// clusterDomain sets --upstream-tls-cluster-domain.
	clusterDomain string
	// caFile is used as --upstream-tls-ca-file in place of a file holding
	// ca, so a test can rewrite it.
	caFile string
}

// newUpstreamTLSRouter builds a router that dials sandboxes over TLS,
// verifying them against ca unless opts.systemRoots is set.
func newUpstreamTLSRouter(t *testing.T, ca *testCA, opts upstreamRouterOpts) *httptest.Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true // httptest binds to 127.0.0.1
	cfg.ProxyTimeout = 5 * time.Second
	cfg.ResponseHeaderTimeout = 2 * time.Second
	cfg.UpstreamTLSMode = config.UpstreamTLSOn
	cfg.UpstreamTLSClusterDomain = opts.clusterDomain
	switch {
	case opts.caFile != "":
		cfg.UpstreamTLSCAFile = opts.caFile
	case !opts.systemRoots:
		cfg.UpstreamTLSCAFile = writeTempFile(t, "ca.crt", ca.pem)
	}
	var caReloader *tlsutil.CAReloader
	if cfg.UpstreamTLSCAFile != "" {
		var err error
		caReloader, err = tlsutil.NewCAReloader(cfg.UpstreamTLSCAFile, logr.Discard(), nil)
		if err != nil {
			t.Fatalf("CA reloader: %v", err)
		}
		if err := caReloader.Start(t.Context()); err != nil {
			t.Fatalf("CA watcher: %v", err)
		}
	}

	var clientCert *tlsutil.CertReloader
	if opts.clientCert {
		certPEM, keyPEM := ca.issue(t, "sandbox-router", nil, x509.ExtKeyUsageClientAuth)
		cfg.UpstreamTLSCertFile = writeTempFile(t, "tls.crt", certPEM)
		cfg.UpstreamTLSKeyFile = writeTempFile(t, "tls.key", keyPEM)
		var err error
		clientCert, err = tlsutil.NewCertReloader(cfg.UpstreamTLSCertFile, cfg.UpstreamTLSKeyFile, logr.Discard(), nil)
		if err != nil {
			t.Fatalf("client cert reloader: %v", err)
		}
	}
	upstreamTLS, err := tlsutil.BuildUpstreamTLS(&cfg, caReloader, clientCert)
	if err != nil {
		t.Fatalf("build upstream TLS: %v", err)
	}
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Logger: logr.Discard(),
		// cmd/main.go always sets Metrics, which wraps the upstream
		// transport, so the tests do too.
		Metrics:     observability.NewMetrics(prometheus.NewRegistry()),
		UpstreamTLS: upstreamTLS,
	}))
	t.Cleanup(router.Close)
	return router
}

// proxyGet sends a GET through router to the backend's address, claiming
// the request is for sandbox id in namespace "test".
func proxyGet(t *testing.T, router, backend *httptest.Server, id string) int {
	t.Helper()
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, router.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderSandboxID, id)
	req.Header.Set(HeaderSandboxNamespace, "test")
	req.Header.Set(HeaderSandboxPodIP, u.Hostname())
	req.Header.Set(HeaderSandboxPort, u.Port())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestIntegration_UpstreamTLSPresentsClientCert(t *testing.T) {
	ca := newTestCA(t)
	backend, seen := newTLSBackend(t, ca, "cluster.local")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{clientCert: true})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	cn, host := seen()
	if cn != "sandbox-router" {
		t.Errorf("backend saw client CN %q, want %q", cn, "sandbox-router")
	}
	u, _ := url.Parse(backend.URL)
	if want := "sb-a.test.svc.cluster.local:" + u.Port(); host != want {
		t.Errorf("backend saw Host %q, want %q", host, want)
	}
}

func TestIntegration_UpstreamTLSRejectsCertForOtherSandbox(t *testing.T) {
	ca := newTestCA(t)
	backend, _ := newTLSBackend(t, ca, "cluster.local")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{clientCert: true})

	// Warm the pool with a verified connection to sb-a's address first, so
	// this also checks that the connection is not reused for sb-b.
	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusOK {
		t.Fatalf("sb-a status = %d, want 200", got)
	}
	if got := proxyGet(t, router, backend, "sb-b"); got != http.StatusBadGateway {
		t.Fatalf("sb-b status = %d, want 502 (certificate is for sb-a)", got)
	}
}

func TestIntegration_UpstreamTLSWithoutClientCertRejectedByBackend(t *testing.T) {
	ca := newTestCA(t)
	backend, _ := newTLSBackend(t, ca, "cluster.local")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (backend requires a client cert)", got)
	}
}

// systemRootsCA is the CA TestIntegration_UpstreamTLSSystemRoots installs as
// the system roots. crypto/x509 loads those only once per process, so a
// repeat run of the test (go test -count=N) must reuse the CA the first run
// installed rather than generate a new one.
var systemRootsCA *testCA

func TestIntegration_UpstreamTLSSystemRoots(t *testing.T) {
	// The test supplies the system roots through SSL_CERT_FILE, which
	// crypto/x509 reads on Linux but not on macOS or Windows.
	if runtime.GOOS != "linux" {
		t.Skip("skipping SSL_CERT_FILE based test on non-linux platform")
	}
	// crypto/x509 reads SSL_CERT_FILE the first time it loads the system
	// roots in a process. No other test in this package verifies against
	// the system roots, so the first run of this test is the one that
	// loads them.
	if systemRootsCA == nil {
		systemRootsCA = newTestCA(t)
		t.Setenv("SSL_CERT_FILE", writeTempFile(t, "system-ca.crt", systemRootsCA.pem))
	}
	ca := systemRootsCA
	backend, _ := newTLSBackend(t, ca, "cluster.local")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{clientCert: true, systemRoots: true})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sandbox cert chains to the system roots)", got)
	}
}

func TestIntegration_UpstreamTLSClusterDomain(t *testing.T) {
	ca := newTestCA(t)
	backend, seen := newTLSBackend(t, ca, "sandboxes.example")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{clientCert: true, clusterDomain: "sandboxes.example"})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	_, host := seen()
	u, _ := url.Parse(backend.URL)
	if want := "sb-a.test.svc.sandboxes.example:" + u.Port(); host != want {
		t.Errorf("backend saw Host %q, want %q", host, want)
	}
}

func TestIntegration_UpstreamTLSDefaultsToClusterDomain(t *testing.T) {
	ca := newTestCA(t)
	backend, _ := newTLSBackend(t, ca, "sandboxes.example")
	router := newUpstreamTLSRouter(t, ca, upstreamRouterOpts{clientCert: true})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (cert names sandboxes.example, router checks cluster.local)", got)
	}
}

func TestIntegration_UpstreamTLSReloadsCA(t *testing.T) {
	oldCA, newCA := newTestCA(t), newTestCA(t)
	backend, _ := newTLSBackend(t, newCA, "cluster.local")
	caFile := writeTempFile(t, "ca.crt", oldCA.pem)
	// newCA also issues the router's client certificate, which the backend
	// requires; only the CA bundle the router verifies against changes.
	router := newUpstreamTLSRouter(t, newCA, upstreamRouterOpts{clientCert: true, caFile: caFile})

	if got := proxyGet(t, router, backend, "sb-a"); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 before rotation (router trusts only the old CA)", got)
	}

	// Replace the bundle atomically: write the new bundle to a file next to
	// it, then rename that file over the original. Kubelet's Secret and
	// ConfigMap projections are atomic too, by swapping a ..data symlink,
	// which the watcher also handles.
	tmp := caFile + ".tmp"
	if err := os.WriteFile(tmp, newCA.pem, 0o600); err != nil {
		t.Fatalf("write new CA: %v", err)
	}
	if err := os.Rename(tmp, caFile); err != nil {
		t.Fatalf("rename new CA: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := proxyGet(t, router, backend, "sb-a")
		if got == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %d, want 200 after rotation", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestUpstreamTLSHandshakeTimeout checks the TLS dial hook against a
// sandbox that accepts the TCP connection but never answers the handshake.
func TestUpstreamTLSHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()

	cfg := config.Defaults()
	upstreamTLS, err := tlsutil.BuildUpstreamTLS(&cfg, nil, nil)
	if err != nil {
		t.Fatalf("build upstream TLS: %v", err)
	}
	tr := defaultTransport(&cfg, upstreamTLS)
	// The hook reads the timeout from the transport on each dial.
	tr.TLSHandshakeTimeout = 100 * time.Millisecond

	ctx := context.WithValue(t.Context(), dialAddrKey{}, ln.Addr().String())
	start := time.Now()
	_, err = tr.DialTLSContext(ctx, "tcp", "sb-a.test.svc.cluster.local:443")
	if err == nil {
		t.Fatalf("handshake with a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dial took %s; the handshake timeout did not apply", elapsed)
	}
	if !strings.Contains(err.Error(), "TLS handshake timeout") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a TLS handshake timeout", err)
	}
	if got := classifyError(err); got != "timeout" {
		t.Fatalf("classifyError = %q, want timeout", got)
	}
	if isRetriableDialError(err) {
		t.Fatalf("handshake timeout is retried as a dial error")
	}

	// The router closed its end of the connection.
	var c net.Conn
	select {
	case c = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("listener never accepted the connection")
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("connection still open after the timeout: %v", err)
	}
}
