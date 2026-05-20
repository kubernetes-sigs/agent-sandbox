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

// Package server coordinates the four HTTP servers the sandbox-router
// runs: plain proxy, TLS proxy, metrics, and health probes.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
)

// Server bundles the four HTTP listeners and their shared lifecycle.
type Server struct {
	log    logr.Logger
	probes *Probes

	proxy     *http.Server // plain HTTP proxy listener (optional)
	proxyTLS  *http.Server // HTTPS proxy listener (optional)
	metrics   *http.Server // /metrics endpoint
	healthSrv *http.Server // /healthz, /readyz

	shutdownTimeout time.Duration
}

// Options bundles the fields New needs to construct a Server.
type Options struct {
	Log             logr.Logger
	Probes          *Probes
	ProxyHandler    http.Handler
	MetricsHandler  http.Handler
	HTTPAddr        string
	HTTPSAddr       string
	MetricsAddr     string
	ProbeAddr       string
	TLSConfig       *tls.Config
	ShutdownTimeout time.Duration

	// Optional tuning knobs applied to the proxy listeners.
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
}

// New assembles a Server from o. At least one of HTTPAddr or HTTPSAddr must
// be non-empty; HTTPSAddr requires TLSConfig.
func New(o Options) (*Server, error) {
	if o.HTTPAddr == "" && o.HTTPSAddr == "" {
		return nil, errors.New("at least one of HTTPAddr or HTTPSAddr is required")
	}
	if o.HTTPSAddr != "" && o.TLSConfig == nil {
		return nil, errors.New("HTTPSAddr requires TLSConfig")
	}
	if o.Probes == nil {
		o.Probes = NewProbes()
	}
	if o.ProxyHandler == nil {
		return nil, errors.New("ProxyHandler is required")
	}

	// Default tuning that mirrors net/http best practice for public listeners.
	if o.ReadHeaderTimeout == 0 {
		o.ReadHeaderTimeout = 10 * time.Second
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 120 * time.Second
	}

	s := &Server{
		log:             o.Log,
		probes:          o.Probes,
		shutdownTimeout: o.ShutdownTimeout,
	}

	if o.HTTPAddr != "" {
		s.proxy = &http.Server{
			Addr:              o.HTTPAddr,
			Handler:           o.ProxyHandler,
			ReadHeaderTimeout: o.ReadHeaderTimeout,
			IdleTimeout:       o.IdleTimeout,
		}
	}
	if o.HTTPSAddr != "" {
		s.proxyTLS = &http.Server{
			Addr:              o.HTTPSAddr,
			Handler:           o.ProxyHandler,
			TLSConfig:         o.TLSConfig,
			ReadHeaderTimeout: o.ReadHeaderTimeout,
			IdleTimeout:       o.IdleTimeout,
		}
	}
	if o.MetricsAddr != "" && o.MetricsHandler != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", o.MetricsHandler)
		s.metrics = &http.Server{
			Addr:              o.MetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: o.ReadHeaderTimeout,
			IdleTimeout:       o.IdleTimeout,
		}
	}
	if o.ProbeAddr != "" {
		s.healthSrv = &http.Server{
			Addr:              o.ProbeAddr,
			Handler:           o.Probes.Mux(),
			ReadHeaderTimeout: o.ReadHeaderTimeout,
			IdleTimeout:       o.IdleTimeout,
		}
	}
	return s, nil
}

// Run starts every configured listener and blocks until ctx is canceled or a
// listener returns an unrecoverable error. On exit it calls Shutdown on every
// server in parallel with the configured shutdown timeout, then returns the
// first non-nil server error (if any).
func (s *Server) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	type listener struct {
		name string
		srv  *http.Server
		tls  bool
	}
	listeners := []listener{
		{"proxy-http", s.proxy, false},
		{"proxy-https", s.proxyTLS, true},
		{"metrics", s.metrics, false},
		{"health", s.healthSrv, false},
	}

	for _, l := range listeners {
		if l.srv == nil {
			continue
		}
		g.Go(func() error {
			s.log.Info("listening", "name", l.name, "addr", l.srv.Addr, "tls", l.tls)
			var err error
			if l.tls {
				// Empty cert/key paths because GetCertificate handles the cert.
				err = l.srv.ListenAndServeTLS("", "")
			} else {
				err = l.srv.ListenAndServe()
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("%s server: %w", l.name, err)
		})
	}

	// Mark ready once listeners are up. Listeners bind synchronously inside
	// ListenAndServe before serving the first request, so the goroutines
	// above transition from "g.Go scheduled" to "Listening" near-instantly.
	// A tiny grace period is not necessary — even if the first request
	// arrives during this window it would just see 503 from /readyz briefly.
	s.probes.MarkReady()

	// Wait for cancellation or first error.
	<-gctx.Done()
	s.probes.MarkUnready()
	s.log.Info("shutdown initiated")

	// Drain phase. Bound each Shutdown by shutdownTimeout.
	shutCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	var shutErr error
	for _, l := range listeners {
		if l.srv == nil {
			continue
		}
		if err := l.srv.Shutdown(shutCtx); err != nil && shutErr == nil {
			shutErr = fmt.Errorf("%s shutdown: %w", l.name, err)
		}
	}

	if err := g.Wait(); err != nil {
		// If a listener failed (not because of ErrServerClosed), prefer that
		// error over the shutdown error.
		return err
	}
	return shutErr
}
