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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
)

func routerPortForwardOpts() Options {
	opts := Options{
		WarmPoolName:        "test-warmpool",
		Namespace:           "default",
		Runtime:             RuntimeSandboxd,
		Connectivity:        ConnectivityRouterPortForward,
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	return opts
}

func TestModeSelection_RouterPortForward_Sandboxd(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.SandboxdRESTPort = 18080
	c, _, _ := newTestSandbox(opts)

	ts, ok := c.connector.strategy.(*tunnelStrategy)
	if !ok || !ts.routerREST {
		t.Fatalf("expected router REST *tunnelStrategy, got %T", c.connector.strategy)
	}
	if !c.connector.routerHeaders {
		t.Error("expected routerHeaders=true so the router can select the sandbox HTTP port")
	}
	if c.connector.serverPort != 18080 {
		t.Errorf("expected X-Sandbox-Port to be the sandboxd REST port 18080, got %d", c.connector.serverPort)
	}
	if got := grpcTarget(c.connector); got != "" {
		t.Errorf("expected no gRPC target, got %s", got)
	}
}

func TestValidation_RouterPortForwardRequiresSandboxd(t *testing.T) {
	opts := Options{
		WarmPoolName: "pool",
		Runtime:      RuntimeLegacyPython,
		Connectivity: ConnectivityRouterPortForward,
		Quiet:        true,
	}
	opts.setDefaults()
	err := validateAllOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("expected router-port-forward to require RuntimeSandboxd, got %v", err)
	}
}

func TestModeSelection_SandboxdPortForward_StaysOnPodTunnel(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.Connectivity = ConnectivityPortForward
	c, _, _ := newTestSandbox(opts)

	if _, ok := c.connector.strategy.(*podTunnelStrategy); !ok {
		t.Fatalf("expected *podTunnelStrategy, got %T", c.connector.strategy)
	}
	if c.connector.routerHeaders {
		t.Error("expected routerHeaders=false for the sandboxd pod tunnel")
	}
}

func TestRouterPortForward_GRPCUnsupported(t *testing.T) {
	c, _, _ := newTestSandbox(routerPortForwardOpts())

	_, err := c.connector.GRPCConn()
	if !errors.Is(err, ErrUnsupportedByRuntime) {
		t.Fatalf("expected ErrUnsupportedByRuntime, got %v", err)
	}
	_, err = c.Run(context.Background(), "echo hi")
	if !errors.Is(err, ErrUnsupportedByRuntime) {
		t.Fatalf("Run: expected ErrUnsupportedByRuntime, got %v", err)
	}
	_, err = c.Commands().Start(context.Background(), "echo hi")
	if !errors.Is(err, ErrUnsupportedByRuntime) {
		t.Fatalf("Start: expected ErrUnsupportedByRuntime, got %v", err)
	}
}

// TestConnector_RouterHeadersReachSandboxdREST stands up a fake router that
// proxies only when X-Sandbox-Port is SandboxdRESTPort, and a fake sandboxd
// behind it. Health must travel that path; Run must still fail.
func TestConnector_RouterHeadersReachSandboxdREST(t *testing.T) {
	const restPort = 18080
	sandboxd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","uptime_seconds":7}`)
	}))
	t.Cleanup(sandboxd.Close)

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerSandboxID) == "" ||
			r.Header.Get(headerSandboxNamespace) != "default" ||
			r.Header.Get(headerSandboxPort) != strconv.Itoa(restPort) {
			http.Error(w, "router rejected headers", http.StatusBadRequest)
			return
		}
		upstream, err := http.NewRequestWithContext(r.Context(), r.Method, sandboxd.URL+r.URL.RequestURI(), http.NoBody)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultClient.Do(upstream)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(router.Close)

	opts := routerPortForwardOpts()
	opts.SandboxdRESTPort = restPort
	sb, _, _ := newTestSandbox(opts)
	sb.connector.mu.Lock()
	sb.connector.baseURL = router.URL
	sb.connector.sandboxID = "sb-kata"
	sb.connector.mu.Unlock()

	health, err := sb.Health(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ok", health.Status)
	require.Equal(t, int64(7), health.UptimeSeconds)

	_, err = sb.Run(context.Background(), "echo hi")
	require.ErrorIs(t, err, ErrUnsupportedByRuntime)
}

func TestRouterTunnelStrategy_Connect_ForwardsRouterHTTP(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-router-svc-abc",
			Namespace: "default",
			Labels:    map[string]string{"kubernetes.io/service-name": "sandbox-router-svc"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Name: "router-pod-abc"},
		}},
	}
	kubeCS := fake.NewSimpleClientset(slice) //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig

	fw := &fakeForwarder{
		ports:       []portforward.ForwardedPort{{Local: 23456, Remote: routerProxyPort}},
		signalReady: true,
		returned:    make(chan struct{}),
	}
	var gotPod string
	var gotSpecs []string
	tracer, svcName := newTracer(Options{TraceServiceName: "sandbox-client-test"})
	s := &tunnelStrategy{
		discoveryClient: kubeCS.DiscoveryV1(),
		namespace:       "default",
		log:             logr.Discard(),
		tracer:          tracer,
		svcName:         svcName,
		routerREST:      true,
		tunnel: &portForwardTunnel{
			name:       "router port-forward",
			coreClient: kubeCS.CoreV1(),
			restConfig: &rest.Config{},
			namespace:  "default",
			pfTimeout:  5 * time.Second,
			log:        logr.Discard(),
			dial: func(podName string) (*trackingDialer, error) {
				gotPod = podName
				return &trackingDialer{}, nil
			},
			newForwarder: func(_ httpstream.Dialer, specs []string, stopChan <-chan struct{}, readyChan chan struct{}, _ io.Writer, errOut io.Writer) (portForwarder, error) {
				gotSpecs = specs
				fw.stopChan = stopChan
				fw.readyChan = readyChan
				fw.errOut = errOut
				return fw, nil
			},
		},
	}

	gotURL, err := s.Connect(context.Background())
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:23456", gotURL)
	require.Equal(t, "router-pod-abc", gotPod)
	require.Equal(t, []string{"0:8080"}, gotSpecs)

	require.NoError(t, s.Close())
	waitForwardReturned(t, fw)
}

func TestRouterTunnelStrategy_Connect_NoEndpoints(t *testing.T) {
	kubeCS := fake.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	tracer, svcName := newTracer(Options{TraceServiceName: "sandbox-client-test"})
	s := &tunnelStrategy{
		discoveryClient: kubeCS.DiscoveryV1(),
		namespace:       "default",
		log:             logr.Discard(),
		tracer:          tracer,
		svcName:         svcName,
		routerREST:      true,
		tunnel: &portForwardTunnel{
			coreClient: kubeCS.CoreV1(),
			restConfig: &rest.Config{},
			log:        logr.Discard(),
		},
	}

	if _, err := s.Connect(context.Background()); err == nil {
		t.Fatal("expected an error when the router has no ready endpoints")
	}
}

// routerSliceIn returns a ready sandbox-router-svc EndpointSlice in namespace.
func routerSliceIn(namespace string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-router-svc-xyz",
			Namespace: namespace,
			Labels:    map[string]string{"kubernetes.io/service-name": "sandbox-router-svc"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Name: "router-pod-xyz"},
		}},
	}
}

// newRouterTunnelForTest builds the strategy through newTunnelStrategy, so
// the Options -> namespace wiring is what gets exercised.
func newRouterTunnelForTest(t *testing.T, opts Options, slice *discoveryv1.EndpointSlice) (*tunnelStrategy, *string) {
	t.Helper()
	kubeCS := fake.NewSimpleClientset(slice) //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	k8s := &K8sHelper{
		CoreClient:      kubeCS.CoreV1(),
		DiscoveryClient: kubeCS.DiscoveryV1(),
		RestConfig:      &rest.Config{},
	}
	opts.Logger = logr.Discard()
	tracer, svcName := newTracer(Options{TraceServiceName: "sandbox-client-test"})
	s := newTunnelStrategy(k8s, opts, tracer, svcName, true)

	gotPod := new(string)
	fw := &fakeForwarder{
		ports:       []portforward.ForwardedPort{{Local: 23457, Remote: routerProxyPort}},
		signalReady: true,
		returned:    make(chan struct{}),
	}
	s.tunnel.dial = func(podName string) (*trackingDialer, error) {
		*gotPod = podName
		return &trackingDialer{}, nil
	}
	s.tunnel.newForwarder = func(_ httpstream.Dialer, _ []string, stopChan <-chan struct{}, readyChan chan struct{}, _ io.Writer, errOut io.Writer) (portForwarder, error) {
		fw.stopChan = stopChan
		fw.readyChan = readyChan
		fw.errOut = errOut
		return fw, nil
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s, gotPod
}

// The shipped manifests run the router in agent-sandbox-system while claims
// default to "default"; RouterNamespace must steer both discovery and the
// port-forward there.
func TestRouterTunnelStrategy_RouterNamespace(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.RouterNamespace = "agent-sandbox-system"
	s, gotPod := newRouterTunnelForTest(t, opts, routerSliceIn("agent-sandbox-system"))

	require.Equal(t, "agent-sandbox-system", s.namespace)
	require.Equal(t, "agent-sandbox-system", s.tunnel.namespace)
	gotURL, err := s.Connect(context.Background())
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:23457", gotURL)
	require.Equal(t, "router-pod-xyz", *gotPod)
}

// Without RouterNamespace the router is looked up in the claim namespace,
// preserving the pre-existing co-location behavior.
func TestRouterTunnelStrategy_RouterNamespaceDefaultsToNamespace(t *testing.T) {
	opts := routerPortForwardOpts()
	s, _ := newRouterTunnelForTest(t, opts, routerSliceIn("agent-sandbox-system"))

	require.Equal(t, "default", s.namespace)
	_, err := s.Connect(context.Background())
	require.ErrorContains(t, err, "no ready endpoints for sandbox-router-svc in namespace default")
}

// Routing headers keep the claim namespace even when the router lives
// elsewhere: the router uses X-Sandbox-Namespace to find the sandbox.
func TestModeSelection_RouterNamespaceKeepsHeaderNamespace(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.Namespace = "team-a"
	opts.RouterNamespace = "agent-sandbox-system"
	c, _, _ := newTestSandbox(opts)

	ts, ok := c.connector.strategy.(*tunnelStrategy)
	require.True(t, ok, "expected *tunnelStrategy, got %T", c.connector.strategy)
	require.Equal(t, "agent-sandbox-system", ts.namespace)
	require.Equal(t, "team-a", c.connector.namespace)
}

// Client defaults its Options once and then swaps Namespace per call, so an
// unset RouterNamespace must follow that per-call Namespace.
func TestRouterNamespace_FollowsPerCallNamespace(t *testing.T) {
	opts := Options{WarmPoolName: "pool", Quiet: true}
	opts.setDefaults()

	perCall := opts
	perCall.Namespace = "team-a"
	require.Equal(t, "team-a", perCall.routerNamespace())

	opts.RouterNamespace = "agent-sandbox-system"
	perCall = opts
	perCall.Namespace = "team-a"
	require.Equal(t, "agent-sandbox-system", perCall.routerNamespace())
}

func TestValidation_RouterNamespaceMustBeDNSLabel(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.RouterNamespace = "Bad_NS"
	err := opts.validateCommon()
	require.ErrorContains(t, err, "RouterNamespace")
}
