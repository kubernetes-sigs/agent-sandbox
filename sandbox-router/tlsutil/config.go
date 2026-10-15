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

// UpstreamTLS holds the TLS settings for the router's connections to
// sandboxes. ClientConfig builds the configuration for each new connection.
type UpstreamTLS struct {
	base *tls.Config
	ca   *CAReloader
}

// BuildUpstreamTLS returns the TLS settings for connections to sandboxes.
// Sandbox certificates are verified against ca's current bundle, or the
// system roots when ca is nil. clientCert supplies the router's client
// certificate and may be nil, in which case none is presented.
func BuildUpstreamTLS(ca *CAReloader, clientCert *CertReloader) *UpstreamTLS {
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	if clientCert != nil {
		base.GetClientCertificate = clientCert.GetClientCertificate
	}
	return &UpstreamTLS{base: base, ca: ca}
}

// ClientConfig returns the configuration for a new connection to a sandbox
// whose certificate must be valid for serverName. A tls.Config must not be
// modified once passed to a TLS function (see its doc), so each connection
// gets its own copy, with RootCAs set to the bundle loaded most recently.
// That lets the CA bundle rotate while crypto/tls still does all of the
// certificate verification, including the extra checks it applies in FIPS
// 140-3 mode.
func (u *UpstreamTLS) ClientConfig(serverName string) *tls.Config {
	tc := u.base.Clone()
	tc.ServerName = serverName
	if u.ca != nil {
		tc.RootCAs = u.ca.Pool()
	}
	return tc
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
