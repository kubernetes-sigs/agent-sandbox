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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
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

// TestConnector_RouterHeadersReachSandboxdREST stands up a fake router that
// proxies only when X-Sandbox-Port is SandboxdRESTPort, and a fake sandboxd
// behind it. Health must travel that path.
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

// fakeGRPCRouter serves HTTP/1.1 and h2c on one port, like the sandbox-router.
// It rejects gRPC calls without X-Sandbox-ID, or with an X-Sandbox-Port other
// than the sandboxd gRPC port, which would fail in a real cluster too.
type fakeGRPCRouter struct {
	port uint16

	mu          sync.Mutex
	grpcHeaders []http.Header
}

func startFakeGRPCRouter(t *testing.T, grpcPort int, svc processv1.ProcessServiceServer) *fakeGRPCRouter {
	t.Helper()
	return startFakeGRPCRouterWith(t, grpcPort, func(s *grpc.Server) {
		processv1.RegisterProcessServiceServer(s, svc)
	})
}

// startFakeGRPCRouterWith registers the caller's services behind the router.
func startFakeGRPCRouterWith(t *testing.T, grpcPort int, register func(*grpc.Server)) *fakeGRPCRouter {
	t.Helper()
	grpcServer := grpc.NewServer()
	register(grpcServer)

	r := &fakeGRPCRouter{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor == 2 && strings.HasPrefix(req.Header.Get("Content-Type"), "application/grpc") {
			r.mu.Lock()
			r.grpcHeaders = append(r.grpcHeaders, req.Header.Clone())
			r.mu.Unlock()
			if req.Header.Get(headerSandboxID) == "" || req.Header.Get(headerSandboxPort) != strconv.Itoa(grpcPort) {
				// A trailers-only response, as the router sends for a bad route.
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Grpc-Status", strconv.Itoa(int(codes.InvalidArgument)))
				w.Header().Set("Grpc-Message", "router rejected routing headers")
				w.WriteHeader(http.StatusOK)
				return
			}
			grpcServer.ServeHTTP(w, req)
			return
		}
		if req.URL.Path == "/v1/health" {
			_, _ = io.WriteString(w, `{"status":"ok","uptime_seconds":1}`)
			return
		}
		http.NotFound(w, req)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		grpcServer.Stop()
	})
	r.port = uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
	return r
}

// lastGRPCHeaders returns the headers of the most recent gRPC call.
func (r *fakeGRPCRouter) lastGRPCHeaders(t *testing.T) http.Header {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.grpcHeaders, "expected the router to receive a gRPC call")
	return r.grpcHeaders[len(r.grpcHeaders)-1]
}

// newRouterPortForwardSandbox returns a router-port-forward Sandbox whose fake
// tunnel reports routerLocal as its local port, so calls go straight to the
// listener on routerLocal.
func newRouterPortForwardSandbox(t *testing.T, routerLocal uint16) (*Sandbox, *fakeForwarder, *[]string) {
	t.Helper()
	return newRouterPortForwardSandboxWith(t, routerLocal, routerPortForwardOpts())
}

// newRouterPortForwardSandboxWith is newRouterPortForwardSandbox with
// caller-supplied Options.
func newRouterPortForwardSandboxWith(t *testing.T, routerLocal uint16, opts Options) (*Sandbox, *fakeForwarder, *[]string) {
	t.Helper()
	opts.RequestTimeout = 5 * time.Second
	opts.PerAttemptTimeout = 2 * time.Second
	sb, _, _ := newTestSandbox(opts)

	ts, ok := sb.connector.strategy.(*tunnelStrategy)
	require.True(t, ok, "expected *tunnelStrategy, got %T", sb.connector.strategy)
	kubeCS := fake.NewSimpleClientset(routerSliceIn("default")) //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	ts.discoveryClient = kubeCS.DiscoveryV1()
	ts.tunnel.coreClient = kubeCS.CoreV1()
	ts.tunnel.restConfig = &rest.Config{}

	fw := &fakeForwarder{
		ports:       []portforward.ForwardedPort{{Local: routerLocal, Remote: routerProxyPort}},
		signalReady: true,
		die:         make(chan error, 1),
		returned:    make(chan struct{}),
	}
	gotSpecs := new([]string)
	ts.tunnel.dial = func(string) (*trackingDialer, error) { return &trackingDialer{}, nil }
	ts.tunnel.newForwarder = func(_ httpstream.Dialer, specs []string, stopChan <-chan struct{}, readyChan chan struct{}, _ io.Writer, errOut io.Writer) (portForwarder, error) {
		*gotSpecs = specs
		fw.stopChan = stopChan
		fw.readyChan = readyChan
		fw.errOut = errOut
		return fw, nil
	}

	sb.connector.SetIdentity("sb-kata")
	sb.connector.SetPodIP("10.244.0.7")

	sb.connector.mu.Lock()
	sb.connector.backoffScale = 0.001
	sb.connector.mu.Unlock()
	t.Cleanup(func() { _ = sb.connector.Close() })
	return sb, fw, gotSpecs
}

// Connect must reuse the router forward's local port as the gRPC target
// instead of opening a second forward.
func TestRouterPortForward_ConnectPublishesGRPCTarget(t *testing.T) {
	sb, fw, gotSpecs := newRouterPortForwardSandbox(t, 23458)
	require.NoError(t, sb.connector.Connect(context.Background()))
	require.Equal(t, "http://127.0.0.1:23458", sb.connector.BaseURL())
	require.Equal(t, "127.0.0.1:23458", grpcTarget(sb.connector))
	require.Equal(t, []string{fmt.Sprintf("0:%d", routerProxyPort)}, *gotSpecs,
		"gRPC must share the router forward rather than open its own")

	_, err := sb.connector.GRPCConn()
	require.NoError(t, err)

	require.NoError(t, sb.connector.Close())
	waitForwardReturned(t, fw)
}

func TestRouterPortForward_RunReachesSandboxdThroughRouter(t *testing.T) {
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{Stdout: []byte("hello\n")}}
	opts := routerPortForwardOpts()
	router := startFakeGRPCRouter(t, opts.SandboxdGRPCPort, svc)
	sb, fw, _ := newRouterPortForwardSandbox(t, router.port)
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	result, err := sb.Run(context.Background(), "echo hello")
	require.NoError(t, err)
	require.Equal(t, "hello\n", result.Stdout)
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, []string{"/bin/sh", "-c", "echo hello"}, svc.lastRequest.GetConfig().GetCommand())

	// X-Sandbox-Port must name the gRPC port, not SandboxdRESTPort.
	h := router.lastGRPCHeaders(t)
	require.Equal(t, "sb-kata", h.Get(headerSandboxID))
	require.Equal(t, "default", h.Get(headerSandboxNamespace))
	require.Equal(t, strconv.Itoa(opts.SandboxdGRPCPort), h.Get(headerSandboxPort))
	require.Equal(t, "10.244.0.7", h.Get(headerSandboxPodIP))

	// REST still works over the same tunnel.
	health, err := sb.Health(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ok", health.Status)
}

// The router strips Authorization, so the sandboxd token must travel in
// X-Sandboxd-Token on both REST and gRPC.
func TestRouterPortForward_SandboxdTokenUsesOwnHeader(t *testing.T) {
	opts := routerPortForwardOpts()
	opts.SandboxdAuthToken = "tok"
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{}}
	router := startFakeGRPCRouter(t, opts.SandboxdGRPCPort, svc)
	sb, fw, _ := newRouterPortForwardSandboxWith(t, router.port, opts)
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	_, err := sb.Run(context.Background(), "true")
	require.NoError(t, err)
	h := router.lastGRPCHeaders(t)
	require.Equal(t, "tok", h.Get(headerSandboxdToken))
	require.Empty(t, h.Get("Authorization"))
}

// A custom SandboxdGRPCPort must reach the router as X-Sandbox-Port.
func TestRouterPortForward_RunUsesConfiguredGRPCPort(t *testing.T) {
	const grpcPort = 19090
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{Stdout: []byte("ok")}}
	router := startFakeGRPCRouter(t, grpcPort, svc)

	opts := routerPortForwardOpts()
	opts.SandboxdGRPCPort = grpcPort
	opts.RequestTimeout = 5 * time.Second
	opts.PerAttemptTimeout = 2 * time.Second
	sb, _, _ := newTestSandbox(opts)
	ts := sb.connector.strategy.(*tunnelStrategy)
	kubeCS := fake.NewSimpleClientset(routerSliceIn("default")) //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	ts.discoveryClient = kubeCS.DiscoveryV1()
	ts.tunnel.coreClient = kubeCS.CoreV1()
	ts.tunnel.restConfig = &rest.Config{}
	fw := &fakeForwarder{
		ports:       []portforward.ForwardedPort{{Local: router.port, Remote: routerProxyPort}},
		signalReady: true,
		returned:    make(chan struct{}),
	}
	stub := newTestPortForwardTunnel(fw)
	ts.tunnel.dial = stub.dial
	ts.tunnel.newForwarder = stub.newForwarder
	sb.connector.SetIdentity("sb-kata")
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	result, err := sb.Run(context.Background(), "true")
	require.NoError(t, err)
	require.Equal(t, "ok", result.Stdout)
	require.Equal(t, strconv.Itoa(grpcPort), router.lastGRPCHeaders(t).Get(headerSandboxPort))
}

// Start must deliver events through the router before the process exits.
func TestRouterPortForward_StartStreamsThroughRouter(t *testing.T) {
	ctx := processTestContext(t)
	finish := make(chan struct{})
	svc := &streamingProcessService{start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		if err := stream.Send(processInit()); err != nil {
			return err
		}
		if err := stream.Send(&processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: []byte("early")}}); err != nil {
			return err
		}
		select {
		case <-finish:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		return stream.Send(processExit(3))
	}}
	opts := routerPortForwardOpts()
	router := startFakeGRPCRouter(t, opts.SandboxdGRPCPort, svc)
	sb, fw, _ := newRouterPortForwardSandbox(t, router.port)
	require.NoError(t, sb.connector.Connect(ctx))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Close() })
	require.Equal(t, int32(42), handle.ProcessID())

	event, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStdout, event.Type)
	require.Equal(t, []byte("early"), event.Data)

	close(finish)
	event, err = handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventExit, event.Type)
	require.Equal(t, 3, event.ExitCode)
	_, err = handle.Recv()
	require.ErrorIs(t, err, io.EOF)

	require.Equal(t, strconv.Itoa(opts.SandboxdGRPCPort), router.lastGRPCHeaders(t).Get(headerSandboxPort))
}

// After the router forward dies, gRPC calls must return ErrNotReady instead of
// dialing its dead port, and Connect must publish the new forward's port.
func TestRouterPortForward_TunnelDeathDropsGRPCTarget(t *testing.T) {
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{Stdout: []byte("hi")}}
	opts := routerPortForwardOpts()
	router := startFakeGRPCRouter(t, opts.SandboxdGRPCPort, svc)
	sb, fw, _ := newRouterPortForwardSandbox(t, router.port)
	require.NoError(t, sb.connector.Connect(context.Background()))

	_, err := sb.Run(context.Background(), "echo hi")
	require.NoError(t, err, "Run before tunnel death")

	fw.die <- portforward.ErrLostConnectionToPod
	waitForwardReturned(t, fw)
	require.Eventually(t, func() bool { return grpcTarget(sb.connector) == "" }, 2*time.Second, 10*time.Millisecond,
		"expected the monitor to drop the dead tunnel's gRPC target")

	_, err = sb.Run(context.Background(), "echo hi")
	require.ErrorIs(t, err, ErrNotReady)
	require.ErrorIs(t, err, ErrPortForwardDied)

	ts := sb.connector.strategy.(*tunnelStrategy)
	fw2 := &fakeForwarder{
		ports:       []portforward.ForwardedPort{{Local: router.port, Remote: routerProxyPort}},
		signalReady: true,
		returned:    make(chan struct{}),
	}
	ts.tunnel.newForwarder = newTestPortForwardTunnel(fw2).newForwarder
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw2)
	})
	require.Equal(t, fmt.Sprintf("127.0.0.1:%d", router.port), grpcTarget(sb.connector))
	result, err := sb.Run(context.Background(), "echo hi")
	require.NoError(t, err, "Run after reconnect")
	require.Equal(t, "hi", result.Stdout)
}

// DisablePodIPRouting must drop X-Sandbox-Pod-IP from gRPC calls too.
func TestRouterPortForward_GRPCOmitsPodIPWhenDisabled(t *testing.T) {
	svc := &fakeProcessService{response: &processv1.ExecuteResponse{Stdout: []byte("hi")}}
	opts := routerPortForwardOpts()
	router := startFakeGRPCRouter(t, opts.SandboxdGRPCPort, svc)
	sb, fw, _ := newRouterPortForwardSandbox(t, router.port)
	sb.connector.mu.Lock()
	sb.connector.disablePodIPRouting = true
	sb.connector.mu.Unlock()
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	_, err := sb.Run(context.Background(), "echo hi")
	require.NoError(t, err)
	h := router.lastGRPCHeaders(t)
	require.Equal(t, "sb-kata", h.Get(headerSandboxID))
	require.Empty(t, h.Values(headerSandboxPodIP))
}

// WatchDir must also reach sandboxd through the router.
func TestRouterPortForward_WatchDirThroughRouter(t *testing.T) {
	watcher := newFakeWatcherService()
	watcher.events = []*watcherv1.FileEvent{
		{Type: watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE, Path: "hello.txt"},
	}
	opts := routerPortForwardOpts()
	router := startFakeGRPCRouterWith(t, opts.SandboxdGRPCPort, func(s *grpc.Server) {
		watcherv1.RegisterFileWatcherServiceServer(s, watcher)
	})
	sb, fw, _ := newRouterPortForwardSandbox(t, router.port)
	require.NoError(t, sb.connector.Connect(context.Background()))
	t.Cleanup(func() {
		_ = sb.connector.Close()
		waitForwardReturned(t, fw)
	})

	ch, err := sb.WatchDir(t.Context(), ".", false)
	require.NoError(t, err)
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "channel closed before the first event")
		require.Equal(t, FileEventCreate, ev.Type)
		require.Equal(t, "hello.txt", ev.Path)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a watch event")
	}
	require.Equal(t, strconv.Itoa(opts.SandboxdGRPCPort), router.lastGRPCHeaders(t).Get(headerSandboxPort))
}
