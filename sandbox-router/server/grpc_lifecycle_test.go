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

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/proxy"
)

func TestRun_GRPCDrainsAndClosesUpstream(t *testing.T) {
	f := newGRPCLifecycleFixture(t, 3*time.Second)
	f.cancelServer()
	waitForReadiness(t, f.probes, false)
	select {
	case <-f.backend.canceled:
		t.Fatal("shutdown canceled the upstream before the drain window expired")
	default:
	}
	close(f.backend.release)
	last, err := f.stream.Recv()
	if err != nil || string(last.GetPayload().GetBody()) != "last" {
		t.Fatalf("last event during drain: %v, %v", last, err)
	}
	if _, err := f.stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("normal drain lost OK terminal status: %v", err)
	}
	if got := f.stream.Trailer().Get("finish"); len(got) != 1 || got[0] != "drained" {
		t.Fatalf("normal drain lost terminal metadata: %v", got)
	}
	if err := f.waitServer(); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	f.handler.Close()
	awaitCompletion(t, f.backend.done, "upstream RPC handler")
	awaitCompletion(t, f.proxyReturned, "proxy response reader/handler")
	awaitCompletion(t, f.connectionEnded, "upstream pooled connection")
}

func TestRun_GRPCForcedShutdownReclaimsBothSides(t *testing.T) {
	f := newGRPCLifecycleFixture(t, 50*time.Millisecond)
	f.cancelServer()
	if err := f.waitServer(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced shutdown error: %v", err)
	}
	// Mirror the executable's pool cleanup immediately after Run returns,
	// while canceled upstream work may still be unwinding.
	f.handler.Close()
	if _, err := f.stream.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("forced shutdown fabricated successful terminal status: %v", err)
	}
	waitForReadiness(t, f.probes, false)
	awaitCompletion(t, f.backend.canceled, "upstream cancellation")
	awaitCompletion(t, f.backend.done, "upstream RPC handler")
	awaitCompletion(t, f.proxyReturned, "proxy response reader/handler")
	awaitCompletion(t, f.connectionEnded, "upstream pooled connection")
}

func TestGRPCTerminalCleanupClosesActiveUpstream(t *testing.T) {
	f := newGRPCLifecycleFixture(t, 50*time.Millisecond)
	// Keep the RPC active so cleanup cannot rely on asynchronous stream
	// teardown having already made the HTTP/2 connection idle.
	f.handler.Close()
	awaitCompletion(t, f.connectionEnded, "active upstream connection")
	if _, err := f.stream.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("terminal cleanup fabricated successful terminal status: %v", err)
	}
	awaitCompletion(t, f.backend.canceled, "upstream cancellation")
	awaitCompletion(t, f.backend.done, "upstream RPC handler")
	awaitCompletion(t, f.proxyReturned, "proxy response reader/handler")
}

func TestGRPCIdleCleanupPreservesActiveStream(t *testing.T) {
	f := newGRPCLifecycleFixture(t, 50*time.Millisecond)
	f.handler.CloseIdleConnections()
	close(f.backend.release)
	last, err := f.stream.Recv()
	if err != nil || string(last.GetPayload().GetBody()) != "last" {
		t.Fatalf("idle cleanup interrupted active RPC: %v, %v", last, err)
	}
	if _, err := f.stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("idle cleanup lost OK terminal status: %v", err)
	}
}

type grpcLifecycleBackend struct {
	grpc_testing.UnimplementedTestServiceServer
	release  chan struct{}
	canceled chan struct{}
	done     chan struct{}
}

func (s *grpcLifecycleBackend) StreamingOutputCall(_ *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
	defer close(s.done)
	if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("first")}}); err != nil {
		return fmt.Errorf("send first event: %w", err)
	}
	select {
	case <-s.release:
	case <-stream.Context().Done():
		close(s.canceled)
		return status.FromContextError(stream.Context().Err()).Err()
	}
	stream.SetTrailer(metadata.Pairs("finish", "drained"))
	if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("last")}}); err != nil {
		return fmt.Errorf("send final event during drain: %w", err)
	}
	return nil
}

type grpcConnectionLifecycle struct {
	ended chan struct{}
	once  sync.Once
}

func (*grpcConnectionLifecycle) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (*grpcConnectionLifecycle) HandleRPC(context.Context, stats.RPCStats) {}

func (*grpcConnectionLifecycle) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (s *grpcConnectionLifecycle) HandleConn(_ context.Context, event stats.ConnStats) {
	if _, ok := event.(*stats.ConnEnd); ok {
		s.once.Do(func() { close(s.ended) })
	}
}

type grpcLifecycleFixture struct {
	backend         *grpcLifecycleBackend
	handler         *proxy.Handler
	probes          *Probes
	stream          grpc.ServerStreamingClient[grpc_testing.StreamingOutputCallResponse]
	cancelServer    context.CancelFunc
	waitServer      func() error
	proxyReturned   chan struct{}
	connectionEnded chan struct{}
}

func newGRPCLifecycleFixture(t *testing.T, grace time.Duration) *grpcLifecycleFixture {
	t.Helper()
	f := &grpcLifecycleFixture{
		backend: &grpcLifecycleBackend{
			release: make(chan struct{}), canceled: make(chan struct{}), done: make(chan struct{}),
		},
		probes: NewProbes(), proxyReturned: make(chan struct{}), connectionEnded: make(chan struct{}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := grpc.NewServer(grpc.StatsHandler(&grpcConnectionLifecycle{ended: f.connectionEnded}))
	grpc_testing.RegisterTestServiceServer(upstream, f.backend)
	backendStopped := make(chan struct{})
	go func() { _ = upstream.Serve(ln); close(backendStopped) }()
	t.Cleanup(func() { upstream.Stop(); awaitCompletion(t, backendStopped, "upstream server") })
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	f.handler = proxy.NewHandler(proxy.Options{Config: &cfg, Logger: logr.Discard()})
	t.Cleanup(f.handler.Close)
	addr := unusedTCPAddr(t)
	srv, err := New(Options{
		Log: logr.Discard(), Probes: f.probes, HTTPAddr: addr, ShutdownTimeout: grace,
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(f.proxyReturned)
			f.handler.ServeHTTP(w, r)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.cancelServer, f.waitServer = runTestServer(t, srv)
	waitForReadiness(t, f.probes, true)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-sandbox-id", "box-a", "x-sandbox-port", port, "x-sandbox-pod-ip", host))
	f.stream, err = grpc_testing.NewTestServiceClient(conn).StreamingOutputCall(ctx, &grpc_testing.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.stream.Recv()
	if err != nil || string(first.GetPayload().GetBody()) != "first" {
		t.Fatalf("first event before shutdown: %v, %v", first, err)
	}
	return f
}

func awaitCompletion(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not stop", name)
	}
}
