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
	"errors"
	"fmt"
	"os"

	inttls "sigs.k8s.io/agent-sandbox/internal/tlsutil"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// BuildServerTLS assembles a *tls.Config for the HTTPS server. The reloader
// supplies the live server certificate via GetCertificate. When cfg.MTLSMode
// is not "off", cfg.TLSClientCAFile is loaded and installed as ClientCAs.
//
// The caller is responsible for ensuring reloader is non-nil whenever
// cfg.HTTPSAddr is set; this function does not verify that pre-condition
// because the binary's main() already runs config.Validate before reaching
// here.
func BuildServerTLS(cfg *config.Config, reloader *CertReloader) (*tls.Config, error) {
	if reloader == nil {
		return nil, errors.New("reloader must not be nil")
	}

	minVersion := uint16(tls.VersionTLS12)
	if cfg.TLSMinVersion != "" {
		v, err := inttls.ParseTLSVersion(cfg.TLSMinVersion)
		if err != nil {
			return nil, fmt.Errorf("parse TLS min version: %w", err)
		}
		minVersion = v
	}

	tc := &tls.Config{
		MinVersion:     minVersion,
		GetCertificate: reloader.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}

	// Always parse so a typo fails at startup even when TLS 1.3 would
	// ignore CipherSuites (Go does not let callers configure TLS 1.3 suites).
	if len(cfg.TLSCipherSuites) > 0 {
		ids, err := inttls.ParseCipherSuites(cfg.TLSCipherSuites)
		if err != nil {
			return nil, fmt.Errorf("parse TLS cipher suites: %w", err)
		}
		if minVersion < tls.VersionTLS13 {
			tc.CipherSuites = ids
		}
	}

	switch cfg.MTLSMode {
	case config.MTLSOff:
		tc.ClientAuth = tls.NoClientCert
	case config.MTLSOptional:
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	case config.MTLSRequired:
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, fmt.Errorf("unsupported mtls mode %q", cfg.MTLSMode)
	}

	if cfg.MTLSMode != config.MTLSOff {
		pool, err := LoadCAPool(cfg.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("load client CA: %w", err)
		}
		tc.ClientCAs = pool
	}

	return tc, nil
}

// BuildMetricsTLS assembles a *tls.Config for the metrics endpoint. It shares
// the same certificate (via reloader) and TLS version/cipher settings as the
// proxy, but without mTLS — Prometheus scraping does not present client certs.
func BuildMetricsTLS(cfg *config.Config, reloader *CertReloader) (*tls.Config, error) {
	if reloader == nil {
		return nil, errors.New("reloader must not be nil")
	}

	minVersion := uint16(tls.VersionTLS12)
	if cfg.TLSMinVersion != "" {
		v, err := inttls.ParseTLSVersion(cfg.TLSMinVersion)
		if err != nil {
			return nil, fmt.Errorf("parse TLS min version: %w", err)
		}
		minVersion = v
	}

	tc := &tls.Config{
		MinVersion:     minVersion,
		GetCertificate: reloader.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}

	if len(cfg.TLSCipherSuites) > 0 {
		ids, err := inttls.ParseCipherSuites(cfg.TLSCipherSuites)
		if err != nil {
			return nil, fmt.Errorf("parse TLS cipher suites: %w", err)
		}
		if minVersion < tls.VersionTLS13 {
			tc.CipherSuites = ids
		}
	}

	return tc, nil
}

// BuildUpstreamTLS assembles the *tls.Config the proxy uses to dial sandboxes
// when upstream TLS is on. Sandbox certificates are verified against ca's
// current bundle, or the system roots when ca is nil. clientCert supplies the
// router's client certificate and may be nil, in which case none is
// presented.
//
// ServerName is left unset on purpose: the proxy uses each sandbox's DNS
// name as the host of the outbound URL, so net/http sets ServerName from
// that name on each new connection, and the certificate is checked against
// it even when the connection goes to a resolved Pod IP.
func BuildUpstreamTLS(ca *CAReloader, clientCert *CertReloader) *tls.Config {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != nil {
		// A tls.Config must not be modified once passed to a TLS function
		// (see its doc), and net/http keeps using this one, so RootCAs
		// cannot be updated in place when the bundle rotates. This uses the
		// replacement pattern from the crypto/tls documentation
		// (ExampleConfig_verifyConnection): InsecureSkipVerify turns off the
		// built-in verification, and VerifyConnection verifies the
		// certificate in its place, against the bundle loaded most recently
		// (see verifyAgainst for how, and how that differs from the built-in
		// check). Per Config.VerifyConnection, crypto/tls runs that callback
		// on every handshake, including resumptions, regardless of
		// InsecureSkipVerify. Do not set InsecureSkipVerify without
		// VerifyConnection; on its own it makes the router accept any
		// certificate.
		tc.InsecureSkipVerify = true
		tc.VerifyConnection = verifyAgainst(ca.Pool)
	}
	if clientCert != nil {
		tc.GetClientCertificate = clientCert.GetClientCertificate
	}
	return tc
}

// verifyAgainst returns a tls.Config.VerifyConnection callback built like
// ExampleConfig_verifyConnection in the crypto/tls documentation, which the
// package describes as approximately equivalent to the built-in
// verification. It makes the same kind of x509.Verify call crypto/tls makes
// on the client side, with the roots taken from roots() at each handshake
// (both use the current time, since Config.Time is unset). That one call
// checks that:
//
//   - the leaf chains to one of those roots, through the intermediates the
//     server sent;
//   - the chain allows server authentication, which x509.VerifyOptions
//     requires when KeyUsages is left empty;
//   - the leaf is valid for cs.ServerName, the SNI value this client sent.
//     net/http fills Config.ServerName from the outbound URL's host, which
//     the proxy sets to the Sandbox's DNS name.
//
// It differs from the built-in check in two ways. The name comes from the
// connection state, which holds the SNI form of Config.ServerName: trailing
// dots are dropped, and an IP address becomes empty, which is rejected below
// rather than matched against IP SANs. The Sandbox names the proxy builds are
// not IP addresses, and x509 ignores a single trailing dot.
//
// The other difference applies only when FIPS 140-3 mode is enabled
// (GODEBUG=fips140=on or only). In that mode the built-in check also
// rejects chains containing a certificate whose key FIPS does not allow,
// such as an RSA key shorter than 2048 bits. crypto/tls does this in an
// unexported function (fipsAllowedChains) that this package cannot call,
// and verifyAgainst does not repeat the rule, so in FIPS mode it can accept
// a chain the built-in check would reject. The rest of the handshake is
// still restricted by FIPS mode.
func verifyAgainst(roots func() *x509.CertPool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("server presented no certificate")
		}
		// x509 skips the name check for an empty DNSName, which would
		// accept any sandbox certificate the CA signed.
		if cs.ServerName == "" {
			return errors.New("no server name to verify the certificate against")
		}
		// KeyUsages is left empty on purpose: x509 treats an empty list as
		// ExtKeyUsageServerAuth.
		opts := x509.VerifyOptions{
			DNSName:       cs.ServerName,
			Roots:         roots(),
			Intermediates: x509.NewCertPool(),
		}
		for _, c := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(opts)
		return err
	}
}

// LoadCAPool reads a PEM-encoded CA bundle from path and returns a new pool
// containing every parsed certificate. It returns an error if the file is
// empty or contains no parseable certificates.
func LoadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("CA file %s is empty", path)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no parseable certificates in %s", path)
	}
	return pool, nil
}
