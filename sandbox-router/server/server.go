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
	"net"
	"net/http"
	"sync"
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
// listener returns an unrecoverable error.
//
// All listener ports are bound synchronously up front so a bind failure
// surfaces as an immediate error from Run() rather than from an async
// goroutine, and so /readyz only flips to 200 after every port is
// actually accepting connections (no rollout window where the LB sends
// traffic to a not-yet-listening pod).
//
// On exit Shutdown is called concurrently on every server with a shared
// shutdownTimeout so one slow listener cannot consume the whole budget.
func (s *Server) Run(ctx context.Context) error {
	type listener struct {
		name string
		srv  *http.Server
		ln   net.Listener
		tls  bool
	}
	listeners := []listener{
		{"proxy-http", s.proxy, nil, false},
		{"proxy-https", s.proxyTLS, nil, true},
		{"metrics", s.metrics, nil, false},
		{"health", s.healthSrv, nil, false},
	}

	// Pre-bind every listener synchronously. If any bind fails, close the
	// already-bound listeners and return — Run() never advertises
	// readiness or starts serving in that case.
	bound := listeners[:0]
	for _, l := range listeners {
		if l.srv == nil {
			continue
		}
		ln, err := net.Listen("tcp", l.srv.Addr)
		if err != nil {
			for _, b := range bound {
				_ = b.ln.Close()
			}
			return fmt.Errorf("listen %s on %s: %w", l.name, l.srv.Addr, err)
		}
		l.ln = ln
		bound = append(bound, l)
		s.log.Info("listening", "name", l.name, "addr", ln.Addr().String(), "tls", l.tls)
	}

	// Start serving on the pre-bound listeners. Per-iteration loop variable
	// scoping is Go 1.22+ default; no shadow copy needed.
	g, gctx := errgroup.WithContext(ctx)
	for _, l := range bound {
		g.Go(func() error {
			var err error
			if l.tls {
				// Empty cert/key paths because GetCertificate handles the cert.
				err = l.srv.ServeTLS(l.ln, "", "")
			} else {
				err = l.srv.Serve(l.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("%s server: %w", l.name, err)
		})
	}

	// Now that every port is bound, advertise readiness.
	s.probes.MarkReady()

	// Wait for cancellation or first listener error.
	<-gctx.Done()
	s.probes.MarkUnready()
	s.log.Info("shutdown initiated")

	// Drain phase — run Shutdown concurrently across listeners so one slow
	// drain can't eat the whole shutdownTimeout budget.
	shutCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	var (
		shutWg  sync.WaitGroup
		shutMu  sync.Mutex
		shutErr error
	)
	for _, l := range bound {
		shutWg.Go(func() {
			if err := l.srv.Shutdown(shutCtx); err != nil {
				shutMu.Lock()
				if shutErr == nil {
					shutErr = fmt.Errorf("%s shutdown: %w", l.name, err)
				}
				shutMu.Unlock()
			}
		})
	}
	shutWg.Wait()

	if err := g.Wait(); err != nil {
		// If a listener failed for any reason other than ErrServerClosed,
		// surface that instead of the shutdown error.
		return err
	}
	return shutErr
}
