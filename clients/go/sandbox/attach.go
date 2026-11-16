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

package sandbox

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
)

// AttachOptions configures Attach.
type AttachOptions struct {
	// Headers are sent on every REST request and, with lower-case names, as
	// metadata on every gRPC call: for example the Authorization the
	// endpoint in front of the sandbox expects.
	Headers http.Header

	// Routing, when set, sends the sandbox-router's X-Sandbox-* headers, with
	// X-Sandbox-Port set to sandboxd's REST or gRPC port per call. Use it
	// when the URL reaches a sandbox-router (for example through a Gateway).
	Routing *AttachRouting

	// TLSConfig is used for gRPC when the URL is https. Nil uses the
	// system's roots. REST uses HTTPTransport.
	TLSConfig *tls.Config

	// HTTPTransport, RequestTimeout, PerAttemptTimeout, MaxDownloadSize,
	// MaxUploadSize, Logger, TracerProvider and TraceServiceName behave as
	// the Options fields of the same names.
	HTTPTransport     http.RoundTripper
	RequestTimeout    time.Duration
	PerAttemptTimeout time.Duration
	MaxDownloadSize   int64
	MaxUploadSize     int64
	Logger            logr.Logger
	TracerProvider    trace.TracerProvider
	TraceServiceName  string
}

// AttachRouting names the sandbox for the sandbox-router.
type AttachRouting struct {
	// SandboxName is sent as X-Sandbox-ID. Required.
	SandboxName string
	// Namespace is sent as X-Sandbox-Namespace. Default: "default".
	Namespace string
	// RESTPort and GRPCPort are sandboxd's ports, sent as X-Sandbox-Port.
	// Defaults: 8080 and 9090.
	RESTPort int
	GRPCPort int
}

// Attach returns a handle to a sandbox that already exists and runs sandboxd,
// reached at baseURL, without the Kubernetes API. The URL serves sandboxd's
// REST API and, on the same host and port, its gRPC services; an https URL is
// dialed with TLS for both.
//
// The handle's Commands, Files, Watchers, Health and Metadata work as on an
// opened Sandbox. It owns no lifecycle: Open, Suspend, Resume and
// IsSuspended return ErrAttached, and Close only closes the connections.
// Use it when something else creates the sandbox (a platform API, an
// operator) and hands the caller its address and credentials.
func Attach(ctx context.Context, baseURL string, opts AttachOptions) (*Sandbox, error) {
	target, useTLS, err := attachGRPCTarget(baseURL)
	if err != nil {
		return nil, err
	}
	if opts.Routing != nil && opts.Routing.SandboxName == "" {
		return nil, errors.New("sandbox: attach: Routing.SandboxName is required")
	}

	o := Options{
		Runtime:           RuntimeSandboxd,
		APIURL:            baseURL,
		HTTPTransport:     opts.HTTPTransport,
		RequestTimeout:    opts.RequestTimeout,
		PerAttemptTimeout: opts.PerAttemptTimeout,
		MaxDownloadSize:   opts.MaxDownloadSize,
		MaxUploadSize:     opts.MaxUploadSize,
		Logger:            opts.Logger,
		TracerProvider:    opts.TracerProvider,
		TraceServiceName:  opts.TraceServiceName,
	}
	if opts.Routing != nil {
		o.Namespace = opts.Routing.Namespace
		o.SandboxdRESTPort = opts.Routing.RESTPort
		o.SandboxdGRPCPort = opts.Routing.GRPCPort
	}
	o.setDefaults()

	tracer, svcName := newTracer(o)
	conn := newConnector(connectorConfig{
		Strategy:          &DirectStrategy{URL: baseURL},
		Namespace:         o.Namespace,
		ServerPort:        o.SandboxdRESTPort,
		SandboxdGRPCPort:  o.SandboxdGRPCPort,
		RouterHeaders:     opts.Routing != nil,
		RequestTimeout:    o.RequestTimeout,
		PerAttemptTimeout: o.PerAttemptTimeout,
		HTTPTransport:     o.HTTPTransport,
		// Attach has no pod IP to route by.
		DisablePodIPRouting: true,
		Log:                 o.Logger,
		Tracer:              tracer,
		TraceServiceName:    svcName,
	})
	conn.headers = opts.Headers.Clone()
	conn.grpcRouterHeaders = opts.Routing != nil
	if useTLS {
		conn.grpcTLS = &tls.Config{MinVersion: tls.VersionTLS12}
		if opts.TLSConfig != nil {
			conn.grpcTLS = opts.TLSConfig.Clone()
		}
	}
	if opts.Routing != nil {
		conn.SetIdentity(opts.Routing.SandboxName)
	}
	if err := conn.Connect(ctx); err != nil {
		return nil, err
	}
	conn.SetGRPCTarget(target)

	s := &Sandbox{
		attached:         true,
		connector:        conn,
		opts:             o,
		log:              o.Logger,
		lifecycleSem:     make(chan struct{}, 1),
		inflightOps:      &sync.WaitGroup{},
		tracer:           tracer,
		traceServiceName: svcName,
	}
	if opts.Routing != nil {
		s.sandboxName = opts.Routing.SandboxName
	}
	s.initFacades()
	return s, nil
}

// attachGRPCTarget derives the gRPC dial target (host:port) from an http or
// https base URL, and reports whether it uses TLS.
func attachGRPCTarget(baseURL string) (target string, useTLS bool, err error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: attach: invalid URL: %w", err)
	}
	port := u.Port()
	switch u.Scheme {
	case "https":
		useTLS = true
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", false, fmt.Errorf("sandbox: attach: URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", false, errors.New("sandbox: attach: URL has no host")
	}
	return net.JoinHostPort(u.Hostname(), port), useTLS, nil
}
