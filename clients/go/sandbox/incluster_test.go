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
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// inClusterTestOpts returns options selecting the in-cluster sandboxd
// transport. It cannot reuse defaultTestOpts, which sets APIURL — that
// combination is rejected by validation.
func inClusterTestOpts() Options {
	opts := Options{
		WarmPoolName:        "test-warmpool",
		Namespace:           "default",
		Runtime:             RuntimeSandboxd,
		Connectivity:        ConnectivityInCluster,
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	return opts
}

// legacyInClusterTestOpts selects the in-cluster transport for the legacy
// python runtime, which is reached on ServerPort and has no gRPC service.
func legacyInClusterTestOpts() Options {
	opts := Options{
		WarmPoolName:        "test-warmpool",
		Namespace:           "default",
		Runtime:             RuntimeLegacyPython,
		Connectivity:        ConnectivityInCluster,
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	return opts
}

// newInClusterStrategy builds a bare strategy wired to a fresh connector,
// bypassing Sandbox construction. Connect starts a span, so it needs a real
// (noop) tracer.
func newInClusterStrategy(getPodIP func() string) (*inClusterStrategy, *connector) {
	return newInClusterStrategyPorts(getPodIP, 8080, 9090)
}

// newInClusterStrategyPorts is the same with explicit ports; grpcPort 0 models
// a runtime with no gRPC service (the legacy runtime).
func newInClusterStrategyPorts(getPodIP func() string, httpPort, grpcPort int) (*inClusterStrategy, *connector) {
	tracer, svcName := newTracer(Options{TraceServiceName: "sandbox-client-test"})
	conn := newConnector(connectorConfig{Log: logr.Discard(), Tracer: tracer, TraceServiceName: svcName})
	return &inClusterStrategy{
		httpPort:  httpPort,
		grpcPort:  grpcPort,
		log:       logr.Discard(),
		tracer:    tracer,
		svcName:   svcName,
		getPodIP:  getPodIP,
		connector: conn,
	}, conn
}

// grpcTarget reads the connector's published gRPC dial address under its lock.
func grpcTarget(c *connector) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grpcTarget
}

// ---------------------------------------------------------------------------
// Mode selection
// ---------------------------------------------------------------------------

func TestModeSelection_InCluster(t *testing.T) {
	opts := inClusterTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if _, ok := c.connector.strategy.(*inClusterStrategy); !ok {
		t.Fatalf("expected *inClusterStrategy, got %T", c.connector.strategy)
	}

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	// readySandbox reports PodIPs 10.244.0.42; ports default to 8080/9090.
	if got, want := c.connector.BaseURL(), "http://10.244.0.42:8080"; got != want {
		t.Errorf("expected baseURL=%s, got %s", want, got)
	}
	if got, want := grpcTarget(c.connector), "10.244.0.42:9090"; got != want {
		t.Errorf("expected grpcTarget=%s, got %s", want, got)
	}
}

func TestModeSelection_InCluster_CustomPorts(t *testing.T) {
	opts := inClusterTestOpts()
	opts.SandboxdRESTPort = 18080
	opts.SandboxdGRPCPort = 19090
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if got, want := c.connector.BaseURL(), "http://10.244.0.42:18080"; got != want {
		t.Errorf("expected baseURL=%s, got %s", want, got)
	}
	if got, want := grpcTarget(c.connector), "10.244.0.42:19090"; got != want {
		t.Errorf("expected grpcTarget=%s, got %s", want, got)
	}
}

// Router headers identify a sandbox to the sandbox-router, which is not in
// the path here: the request goes to the pod. That holds for the legacy
// runtime too, which is the only one that would otherwise send them.
func TestInCluster_DoesNotSendRouterHeaders(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"sandboxd", inClusterTestOpts()},
		{"legacy runtime", legacyInClusterTestOpts()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newTestSandbox(tc.opts)
			if c.connector.routerHeaders {
				t.Error("expected routerHeaders=false for the in-cluster transport")
			}
		})
	}
}

// The legacy runtime is reached on ServerPort and serves no gRPC, so no gRPC
// target should be published.
func TestModeSelection_InCluster_LegacyRuntime(t *testing.T) {
	opts := legacyInClusterTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if _, ok := c.connector.strategy.(*inClusterStrategy); !ok {
		t.Fatalf("expected *inClusterStrategy, got %T", c.connector.strategy)
	}

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if got, want := c.connector.BaseURL(), "http://10.244.0.42:8888"; got != want {
		t.Errorf("expected baseURL=%s, got %s", want, got)
	}
	if got := grpcTarget(c.connector); got != "" {
		t.Errorf("expected no gRPC target for the legacy runtime, got %s", got)
	}
}

func TestModeSelection_InCluster_LegacyRuntime_CustomServerPort(t *testing.T) {
	opts := legacyInClusterTestOpts()
	opts.ServerPort = 3000
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if got, want := c.connector.BaseURL(), "http://10.244.0.42:3000"; got != want {
		t.Errorf("expected baseURL=%s, got %s", want, got)
	}
}

// The legacy runtime must keep sending router headers on every transport that
// actually goes through the sandbox-router.
func TestPortForward_LegacyRuntime_StillSendsRouterHeaders(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)
	if !c.connector.routerHeaders {
		t.Error("expected routerHeaders=true for the legacy runtime off the in-cluster path")
	}
}

// ---------------------------------------------------------------------------
// Strategy behavior
// ---------------------------------------------------------------------------

func TestInClusterStrategy_Connect(t *testing.T) {
	cases := []struct {
		name           string
		podIP          string
		wantURL        string
		wantGRPCTarget string
	}{
		{"IPv4", "10.244.0.42", "http://10.244.0.42:8080", "10.244.0.42:9090"},
		{"IPv6 is bracketed", "2001:db8::1", "http://[2001:db8::1]:8080", "[2001:db8::1]:9090"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, conn := newInClusterStrategy(func() string { return tc.podIP })

			gotURL, err := s.Connect(context.Background())
			if err != nil {
				t.Fatalf("Connect() error: %v", err)
			}
			if gotURL != tc.wantURL {
				t.Errorf("expected baseURL=%s, got %s", tc.wantURL, gotURL)
			}
			if got := grpcTarget(conn); got != tc.wantGRPCTarget {
				t.Errorf("expected grpcTarget=%s, got %s", tc.wantGRPCTarget, got)
			}
		})
	}
}

func TestInClusterStrategy_Connect_NoGRPCPort(t *testing.T) {
	s, conn := newInClusterStrategyPorts(func() string { return "10.244.0.42" }, 8888, 0)

	gotURL, err := s.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	if want := "http://10.244.0.42:8888"; gotURL != want {
		t.Errorf("expected baseURL=%s, got %s", want, gotURL)
	}
	if got := grpcTarget(conn); got != "" {
		t.Errorf("expected no gRPC target when grpcPort is 0, got %s", got)
	}
}

func TestInClusterStrategy_Connect_UnresolvedPodIP(t *testing.T) {
	cases := []struct {
		name     string
		getPodIP func() string
	}{
		{"nil closure", nil},
		{"empty pod IP", func() string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, conn := newInClusterStrategy(tc.getPodIP)

			if _, err := s.Connect(context.Background()); err == nil {
				t.Fatal("expected an error when the pod IP is unresolved")
			}
			if got := grpcTarget(conn); got != "" {
				t.Errorf("expected no gRPC target to be published, got %s", got)
			}
		})
	}
}

// A Connect that fails after an earlier success leaves the previously
// published gRPC target in place, matching podTunnelStrategy: only a
// successful Connect republishes it, and connector.Close clears it. Pinned
// here so the behavior is deliberate rather than incidental.
func TestInClusterStrategy_Connect_KeepsTargetWhenLaterConnectFails(t *testing.T) {
	podIP := "10.244.0.42"
	s, conn := newInClusterStrategy(func() string { return podIP })

	if _, err := s.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	if got, want := grpcTarget(conn), "10.244.0.42:9090"; got != want {
		t.Fatalf("expected grpcTarget=%s after the first Connect, got %s", want, got)
	}

	// The pod went away: Connect re-reads the closure and must fail.
	podIP = ""
	if _, err := s.Connect(context.Background()); err == nil {
		t.Fatal("expected an error once the pod IP is gone")
	}
	if got, want := grpcTarget(conn), "10.244.0.42:9090"; got != want {
		t.Errorf("expected the previous grpcTarget %s to survive a failed Connect, got %s", want, got)
	}
}

// Nothing signals transport death on this path, so a rescheduled pod is only
// picked up when the caller reconnects. Connect must therefore re-read the
// pod IP rather than caching the address from a previous call.
func TestInClusterStrategy_Connect_RefreshesPodIP(t *testing.T) {
	podIP := "10.244.0.42"
	s, conn := newInClusterStrategy(func() string { return podIP })

	if _, err := s.Connect(context.Background()); err != nil {
		t.Fatalf("first Connect() error: %v", err)
	}

	podIP = "10.244.1.7"
	gotURL, err := s.Connect(context.Background())
	if err != nil {
		t.Fatalf("second Connect() error: %v", err)
	}
	if want := "http://10.244.1.7:8080"; gotURL != want {
		t.Errorf("expected baseURL=%s after reschedule, got %s", want, gotURL)
	}
	if got, want := grpcTarget(conn), "10.244.1.7:9090"; got != want {
		t.Errorf("expected grpcTarget=%s after reschedule, got %s", want, got)
	}
}

func TestInClusterStrategy_Close_IsNoOp(t *testing.T) {
	s, _ := newInClusterStrategy(nil)
	if err := s.Close(); err != nil {
		t.Errorf("first Close() error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidation_Connectivity(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{
			name: "in-cluster with sandboxd",
			opts: Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, Connectivity: ConnectivityInCluster},
		},
		{
			name: "port-forward with sandboxd",
			opts: Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, Connectivity: ConnectivityPortForward},
		},
		{
			name: "port-forward with legacy runtime",
			opts: Options{WarmPoolName: "pool", Runtime: RuntimeLegacyPython, Connectivity: ConnectivityPortForward},
		},
		{
			name: "in-cluster with legacy runtime",
			opts: Options{WarmPoolName: "pool", Runtime: RuntimeLegacyPython, Connectivity: ConnectivityInCluster},
		},
		{
			name:    "in-cluster with legacy runtime and GatewayName",
			opts:    Options{WarmPoolName: "pool", Runtime: RuntimeLegacyPython, Connectivity: ConnectivityInCluster, GatewayName: "gw"},
			wantErr: true,
		},
		{
			name:    "in-cluster with legacy runtime and APIURL",
			opts:    Options{WarmPoolName: "pool", Runtime: RuntimeLegacyPython, Connectivity: ConnectivityInCluster, APIURL: "http://localhost:9999"},
			wantErr: true,
		},
		{
			name:    "in-cluster with APIURL",
			opts:    Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, Connectivity: ConnectivityInCluster, APIURL: "http://localhost:9999"},
			wantErr: true,
		},
		{
			name:    "in-cluster with GatewayName",
			opts:    Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, Connectivity: ConnectivityInCluster, GatewayName: "gw"},
			wantErr: true,
		},
		{
			name:    "unknown Connectivity",
			opts:    Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, Connectivity: "sidecar"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Quiet = true
			tc.opts.setDefaults()
			err := validateAllOptions(&tc.opts)
			if tc.wantErr && err == nil {
				t.Error("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no validation error, got %v", err)
			}
		})
	}
}

// APIURL is the documented sandboxd escape hatch and must keep working; only
// ConnectivityInCluster conflicts with it.
func TestValidation_APIURLStillAllowedWithSandboxdDefault(t *testing.T) {
	opts := Options{WarmPoolName: "pool", Runtime: RuntimeSandboxd, APIURL: "http://localhost:9999", Quiet: true}
	opts.setDefaults()
	if opts.Connectivity != ConnectivityPortForward {
		t.Fatalf("expected Connectivity to default to %q, got %q", ConnectivityPortForward, opts.Connectivity)
	}
	if err := validateAllOptions(&opts); err != nil {
		t.Errorf("expected APIURL to remain valid with the default connectivity, got %v", err)
	}
}
