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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

type grpcEchoService struct {
	grpc_testing.UnimplementedTestServiceServer
	unary  func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error)
	output func(*grpc_testing.StreamingOutputCallRequest, grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error
}

func (s *grpcEchoService) UnaryCall(ctx context.Context, req *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
	if s.unary != nil {
		return s.unary(ctx, req)
	}
	return &grpc_testing.SimpleResponse{Payload: req.Payload}, nil
}

func (s *grpcEchoService) StreamingOutputCall(req *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
	return s.output(req, stream)
}

func (s *grpcEchoService) StreamingInputCall(stream grpc.ClientStreamingServer[grpc_testing.StreamingInputCallRequest, grpc_testing.StreamingInputCallResponse]) error {
	var size int32
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&grpc_testing.StreamingInputCallResponse{AggregatedPayloadSize: size})
		}
		if err != nil {
			return err
		}
		size += int32(len(req.GetPayload().GetBody()))
	}
}

func (s *grpcEchoService) FullDuplexCall(stream grpc.BidiStreamingServer[grpc_testing.StreamingOutputCallRequest, grpc_testing.StreamingOutputCallResponse]) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: req.Payload}); err != nil {
			return err
		}
	}
}

func grpcBackend(t *testing.T, service grpc_testing.TestServiceServer) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(s, service)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func grpcRouterConn(t *testing.T, opts Options, useTLS bool) *grpc.ClientConn {
	t.Helper()
	if opts.Config == nil {
		cfg := config.Defaults()
		cfg.AllowLoopbackPodIP = true
		opts.Config = &cfg
	}
	opts.Logger = logr.Discard()
	handler := NewHandler(opts)
	t.Cleanup(handler.CloseIdleConnections)
	return grpcConnToHandler(t, handler, useTLS)
}

func grpcConnToHandler(t *testing.T, handler http.Handler, useTLS bool) *grpc.ClientConn {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.Config.Protocols = new(http.Protocols)
	s.Config.Protocols.SetHTTP1(true)
	s.Config.Protocols.SetUnencryptedHTTP2(true)
	creds := insecure.NewCredentials()
	if useTLS {
		s.EnableHTTP2 = true
		s.StartTLS()
		roots := x509.NewCertPool()
		roots.AddCert(s.Certificate())
		creds = credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	addr := strings.TrimPrefix(strings.TrimPrefix(s.URL, "http://"), "https://")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func grpcRouterClient(t *testing.T, opts Options) grpc_testing.TestServiceClient {
	return grpc_testing.NewTestServiceClient(grpcRouterConn(t, opts, false))
}

func TestGRPCTLSInbound(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	client := grpc_testing.NewTestServiceClient(grpcRouterConn(t, Options{}, true))
	_, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{})
	if err != nil {
		t.Fatalf("verified TLS ingress to h2c backend: %v", err)
	}
}

func grpcTargetContext(t *testing.T, host, port string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-sandbox-id", "box-a", "x-sandbox-port", port, "x-sandbox-pod-ip", host,
	))
}

func TestGRPCUnaryRouting(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	client := grpcRouterClient(t, Options{})
	res, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{
		Payload: &grpc_testing.Payload{Body: []byte("through-router")},
	})
	if err != nil {
		t.Fatalf("unary through router: %v", err)
	}
	if got := string(res.GetPayload().GetBody()); got != "through-router" {
		t.Fatalf("payload = %q, want through-router", got)
	}
}

func TestGRPCTrailersOnlyStatus(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	client := grpcRouterClient(t, Options{})
	_, err := client.EmptyCall(grpcTargetContext(t, host, port), &grpc_testing.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("trailers-only status: %v", err)
	}
}

func TestGRPCRoutingErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   metadata.MD
		err  error
		code codes.Code
	}{
		{"missing id", metadata.Pairs("x-sandbox-port", "9090"), nil, codes.InvalidArgument},
		{"missing port", metadata.Pairs("x-sandbox-id", "box-a"), nil, codes.InvalidArgument},
		{"invalid port", metadata.Pairs("x-sandbox-id", "box-a", "x-sandbox-port", "0"), nil, codes.InvalidArgument},
		{"unauthenticated", metadata.Pairs("x-sandbox-id", "box-a", "x-sandbox-port", "9090"), authz.ErrUnauthenticated, codes.Unauthenticated},
		{"forbidden", metadata.Pairs("x-sandbox-id", "box-a", "x-sandbox-port", "9090"), authz.ErrForbidden, codes.PermissionDenied},
		{"internal", metadata.Pairs("x-sandbox-id", "box-a", "x-sandbox-port", "9090"), errors.New("authorizer unavailable"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := grpcRouterClient(t, Options{Authorizer: &recordingAuthz{err: tc.err}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := client.UnaryCall(metadata.NewOutgoingContext(ctx, tc.md), &grpc_testing.SimpleRequest{})
			if status.Code(err) != tc.code {
				t.Fatalf("status = %v, want %v", err, tc.code)
			}
			if tc.err != nil && status.Convert(err).Message() != tc.err.Error() {
				t.Fatalf("not a native error: %v", err)
			}
		})
	}
}

func TestGRPCStreamingShapes(t *testing.T) {
	release := make(chan struct{})
	svc := &grpcEchoService{output: func(_ *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
		if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("first")}}); err != nil {
			return err
		}
		select {
		case <-release:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		return stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("last")}})
	}}
	host, port := grpcBackend(t, svc)
	client := grpcRouterClient(t, Options{})
	t.Run("server streaming", func(t *testing.T) {
		stream, err := client.StreamingOutputCall(grpcTargetContext(t, host, port), &grpc_testing.StreamingOutputCallRequest{})
		if err != nil {
			t.Fatal(err)
		}
		first, err := stream.Recv()
		if err != nil || string(first.GetPayload().GetBody()) != "first" {
			t.Fatalf("first event: %v, %v", first, err)
		}
		close(release)
		last, err := stream.Recv()
		if err != nil || string(last.GetPayload().GetBody()) != "last" {
			t.Fatalf("last event: %v, %v", last, err)
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("terminal status: %v", err)
		}
	})
	t.Run("client streaming", func(t *testing.T) {
		stream, err := client.StreamingInputCall(grpcTargetContext(t, host, port))
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"abc", "def"} {
			if err := stream.Send(&grpc_testing.StreamingInputCallRequest{Payload: &grpc_testing.Payload{Body: []byte(body)}}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := stream.CloseAndRecv()
		if err != nil || res.GetAggregatedPayloadSize() != 6 {
			t.Fatalf("upload response: %v, %v", res, err)
		}
	})
	t.Run("bidirectional streaming", func(t *testing.T) {
		stream, err := client.FullDuplexCall(grpcTargetContext(t, host, port))
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"before-close-send", "still-uploading"} {
			if err := stream.Send(&grpc_testing.StreamingOutputCallRequest{Payload: &grpc_testing.Payload{Body: []byte(body)}}); err != nil {
				t.Fatal(err)
			}
			res, err := stream.Recv()
			if err != nil || string(res.GetPayload().GetBody()) != body {
				t.Fatalf("duplex response: %v, %v", res, err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("terminal status: %v", err)
		}
	})
}

func TestGRPCMetadataCompressionAndBackendStatus(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, req *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("authorization")) != 0 {
			return nil, status.Error(codes.Internal, "credential reached backend")
		}
		if err := grpc.SendHeader(ctx, metadata.MD{"echo": md.Get("echo"), "echo-bin": md.Get("echo-bin")}); err != nil {
			return nil, err
		}
		if err := grpc.SetTrailer(ctx, metadata.Pairs("end", "complete")); err != nil {
			return nil, err
		}
		if req.GetResponseStatus() != nil {
			st, err := status.New(codes.Aborted, "backend rejected 100% / +").WithDetails(&emptypb.Empty{})
			if err != nil {
				return nil, err
			}
			return nil, st.Err()
		}
		return &grpc_testing.SimpleResponse{Payload: req.Payload}, nil
	}})
	client := grpcRouterClient(t, Options{})
	ctx := metadata.AppendToOutgoingContext(grpcTargetContext(t, host, port), "authorization", "Bearer test-only", "echo", "one", "echo", "two", "echo-bin", "\x00\xff")
	var headers, trailers metadata.MD
	res, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte("compressed")}}, grpc.UseCompressor("gzip"), grpc.Header(&headers), grpc.Trailer(&trailers))
	if err != nil || string(res.GetPayload().GetBody()) != "compressed" {
		t.Fatalf("compressed unary: %v, %v", res, err)
	}
	if strings.Join(headers.Get("echo"), ",") != "one,two" || strings.Join(headers.Get("echo-bin"), "") != "\x00\xff" || strings.Join(trailers.Get("end"), "") != "complete" {
		t.Fatalf("metadata: headers=%v trailers=%v", headers, trailers)
	}
	_, err = client.UnaryCall(ctx, &grpc_testing.SimpleRequest{ResponseStatus: &grpc_testing.EchoStatus{Code: int32(codes.Aborted)}}, grpc.Trailer(&trailers))
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "backend rejected 100% / +" {
		t.Fatalf("backend status lost: %v", err)
	}
	if details := status.Convert(err).Details(); len(details) != 1 {
		t.Fatalf("grpc-status-details-bin lost: %v", details)
	}
}

func TestGRPCTargetAndScopedTokenArePerRPC(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{unary: func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		return &grpc_testing.SimpleResponse{Payload: &grpc_testing.Payload{Body: []byte("backend-a")}}, nil
	}})
	hostB, portB := grpcBackend(t, &grpcEchoService{unary: func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		return &grpc_testing.SimpleResponse{Payload: &grpc_testing.Payload{Body: []byte("backend-b")}}, nil
	}})
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"uid-a": {PodIP: host, Namespace: "team-a", SandboxName: "box-a"},
		"uid-b": {PodIP: hostB, Namespace: "team-b", SandboxName: "box-b"},
	}}
	client := grpcRouterClient(t, Options{Config: &cfg, Cache: lookup, Authorizer: authorizer})
	for _, tc := range []struct {
		name, ns, uid, port, body string
	}{
		{"box-a", "team-a", "uid-a", port, "backend-a"},
		{"box-b", "team-b", "uid-b", portB, "backend-b"},
		{"box-a", "team-a", "uid-a", port, "backend-a"},
	} {
		token, err := authz.MintScopedTokenV2(privateKey, "current", authz.AuthorizationTarget{
			Namespace: tc.ns, SandboxName: tc.name, SandboxUID: tc.uid,
			Port: mustAtoi(t, tc.port), Method: http.MethodPost, Path: grpc_testing.TestService_UnaryCall_FullMethodName,
		}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(
			"x-sandbox-id", tc.name, "x-sandbox-namespace", tc.ns, "x-sandbox-port", tc.port,
			"x-sandbox-uid", "stale-uid", "authorization", "Bearer "+token,
		))
		res, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte(tc.name)}})
		if err != nil || string(res.GetPayload().GetBody()) != tc.body {
			t.Fatalf("canonical target %s: %v, %v", tc.name, res, err)
		}
		wrong := metadata.AppendToOutgoingContext(ctx, "x-sandbox-pod-ip", host)
		_, err = client.UnaryCall(wrong, &grpc_testing.SimpleRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("v2 must reject noncanonical IP override: %v", err)
		}
		_, err = client.EmptyCall(ctx, &grpc_testing.Empty{})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("v2 must reject another RPC method: %v", err)
		}
		cancel()
	}
}

func TestGRPCIgnoresLegacyHTTPTimeouts(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, req *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		timer := time.NewTimer(80 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return &grpc_testing.SimpleResponse{Payload: req.Payload}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, output: func(_ *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
		if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{}); err != nil {
			return err
		}
		timer := time.NewTimer(80 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return stream.Send(&grpc_testing.StreamingOutputCallResponse{})
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 10 * time.Millisecond
	cfg.ResponseHeaderTimeout = 10 * time.Millisecond
	client := grpcRouterClient(t, Options{Config: &cfg})
	_, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{})
	if err != nil {
		t.Fatalf("legacy HTTP budget truncated gRPC: %v", err)
	}
	stream, err := client.StreamingOutputCall(grpcTargetContext(t, host, port), &grpc_testing.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("legacy HTTP budget truncated stream: %v", err)
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream terminal status: %v", err)
	}
}

func TestGRPCConcurrentTargets(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	hostA, portA := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, _ *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		close(started)
		select {
		case <-release:
			return &grpc_testing.SimpleResponse{Payload: &grpc_testing.Payload{Body: []byte("backend-a")}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	hostB, portB := grpcBackend(t, &grpcEchoService{unary: func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		return &grpc_testing.SimpleResponse{Payload: &grpc_testing.Payload{Body: []byte("backend-b")}}, nil
	}})
	client := grpcRouterClient(t, Options{})
	done := make(chan error, 1)
	go func() {
		res, err := client.UnaryCall(grpcTargetContext(t, hostA, portA), &grpc_testing.SimpleRequest{})
		if err == nil && string(res.GetPayload().GetBody()) != "backend-a" {
			err = fmt.Errorf("A received another target's payload")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("A never started")
	}
	ctx := metadata.AppendToOutgoingContext(grpcTargetContext(t, hostB, portB), "x-sandbox-namespace", "team-b")
	res, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{})
	if err != nil || string(res.GetPayload().GetBody()) != "backend-b" {
		t.Fatalf("B while A is active: %v, %v", res, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGRPCBudgetExpiresDuringAuthorization(t *testing.T) {
	var calls atomic.Int32
	host, port := grpcBackend(t, &grpcEchoService{unary: func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		calls.Add(1)
		return &grpc_testing.SimpleResponse{}, nil
	}})
	client := grpcRouterClient(t, Options{Authorizer: grpcAuthFunc(func(ctx context.Context, _ *http.Request, _ authz.AuthorizationTarget) error {
		<-ctx.Done()
		return ctx.Err()
	})})
	ctx, cancel := context.WithTimeout(grpcTargetContext(t, host, port), 50*time.Millisecond)
	defer cancel()
	_, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{})
	if status.Code(err) != codes.DeadlineExceeded || calls.Load() != 0 {
		t.Fatalf("expired RPC reached backend: calls=%d err=%v", calls.Load(), err)
	}
}

func TestGRPCLargeAndRepeatedBudget(t *testing.T) {
	for _, tc := range []struct {
		values []string
		code   string
	}{
		{[]string{"99999999H"}, "7"},
		{[]string{"1S", "2S"}, "3"},
	} {
		cfg := config.Defaults()
		h := NewHandler(Options{Config: &cfg, Authorizer: &recordingAuthz{err: authz.ErrForbidden}})
		t.Cleanup(h.CloseIdleConnections)
		r := httptest.NewRequest(http.MethodPost, "/service/Method", nil)
		r.ProtoMajor = 2
		r.Header.Set("Content-Type", "application/grpc")
		r.Header.Set(HeaderSandboxID, "box-a")
		r.Header.Set(HeaderSandboxPort, "9090")
		r.Header["Grpc-Timeout"] = tc.values
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Grpc-Status") != tc.code {
			t.Fatalf("budget %v: status %v", tc.values, w.Header())
		}
	}
}

func TestGRPCObservabilityReportsTerminalStatus(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{output: func(_ *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
		if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("before-error")}}); err != nil {
			return err
		}
		return status.Error(codes.DataLoss, "stream failed after output")
	}})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	h := NewHandler(Options{Config: &cfg})
	t.Cleanup(h.CloseIdleConnections)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	logs := make(chan string, 1)
	logger := funcr.New(func(_, message string) { logs <- message }, funcr.Options{})
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	chain := observability.TracingMiddleware(tp.Tracer("test"), propagation.TraceContext{}, logger)(
		metrics.Middleware(observability.AccessLogMiddleware(logger, nil)(h)))
	client := grpc_testing.NewTestServiceClient(grpcConnToHandler(t, chain, false))
	stream, err := client.StreamingOutputCall(grpcTargetContext(t, host, port), &grpc_testing.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || string(first.GetPayload().GetBody()) != "before-error" {
		t.Fatalf("middleware buffered or broke first event: %v, %v", first, err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.DataLoss {
		t.Fatalf("terminal code: %v", err)
	}
	select {
	case line := <-logs:
		if !strings.Contains(line, `"grpc_status"="15"`) {
			t.Fatalf("missing terminal status in log: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("missing access log")
	}
	deadline := time.Now().Add(time.Second)
	for len(recorder.Ended()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans: %d", len(spans))
	}
	attrs := attribute.NewSet(spans[0].Attributes()...)
	if value, ok := attrs.Value("grpc.status_code"); !ok || value.AsString() != "15" {
		t.Fatalf("trace terminal status: %v", value)
	}
}

func TestGRPCAdministratorDeadline(t *testing.T) {
	backendDone := make(chan struct{})
	host, port := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, _ *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		<-ctx.Done()
		close(backendDone)
		return nil, status.FromContextError(ctx.Err()).Err()
	}})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.GRPCProxyTimeout = 100 * time.Millisecond
	client := grpcRouterClient(t, Options{Config: &cfg})
	started := time.Now()
	_, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("administrator cap: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("administrator cap did not bound the call: %s", elapsed)
	}
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("backend RPC did not terminate")
	}
}

type grpcAuthFunc func(context.Context, *http.Request, authz.AuthorizationTarget) error

func (f grpcAuthFunc) Authorize(ctx context.Context, r *http.Request, target authz.AuthorizationTarget) error {
	return f(ctx, r, target)
}

func TestGRPCBudgetIncludesAuthorization(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, _ *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		deadline, _ := ctx.Deadline()
		return &grpc_testing.SimpleResponse{Payload: &grpc_testing.Payload{Body: []byte(deadline.Format(time.RFC3339Nano))}}, nil
	}})
	client := grpcRouterClient(t, Options{Authorizer: grpcAuthFunc(func(ctx context.Context, _ *http.Request, _ authz.AuthorizationTarget) error {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})})
	ctx := grpcTargetContext(t, host, port)
	deadline, _ := ctx.Deadline()
	res, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	backendDeadline, err := time.Parse(time.RFC3339Nano, string(res.GetPayload().GetBody()))
	if err != nil {
		t.Fatal(err)
	}
	if backendDeadline.After(deadline.Add(100 * time.Millisecond)) {
		t.Fatalf("authorization restarted the budget: backend=%s caller=%s", backendDeadline, deadline)
	}
}

func TestGRPCCancelDoesNotCancelOtherRPC(t *testing.T) {
	started, ended := make(chan struct{}), make(chan struct{})
	host, port := grpcBackend(t, &grpcEchoService{unary: func(ctx context.Context, req *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		if string(req.GetPayload().GetBody()) == "wait" {
			close(started)
			<-ctx.Done()
			close(ended)
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return &grpc_testing.SimpleResponse{Payload: req.Payload}, nil
	}})
	client := grpcRouterClient(t, Options{})
	ctx, cancel := context.WithCancel(grpcTargetContext(t, host, port))
	done := make(chan error, 1)
	go func() {
		_, err := client.UnaryCall(ctx, &grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte("wait")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("backend never started")
	}
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	res, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte("survived")}})
	if err != nil || string(res.GetPayload().GetBody()) != "survived" {
		t.Fatalf("other RPC affected: %v, %v", res, err)
	}
}

func TestGRPCRequestBodyLimit(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.MaxRequestBodyBytes = 20
	client := grpcRouterClient(t, Options{Config: &cfg})
	stream, err := client.StreamingInputCall(grpcTargetContext(t, host, port))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_ = stream.Send(&grpc_testing.StreamingInputCallRequest{Payload: &grpc_testing.Payload{Body: []byte("abcdefgh")}})
	}
	_, err = stream.CloseAndRecv()
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("cumulative upload limit: %v", err)
	}
}

func TestGRPCDoesNotReplayBackendOperation(t *testing.T) {
	var executions atomic.Int32
	host, port := grpcBackend(t, &grpcEchoService{unary: func(_ context.Context, _ *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
		executions.Add(1)
		return nil, status.Error(codes.Unavailable, "operation executed but reply unavailable")
	}})
	client := grpcRouterClient(t, Options{})
	_, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{})
	if status.Code(err) != codes.Unavailable || executions.Load() != 1 {
		t.Fatalf("operation replayed: executions=%d err=%v", executions.Load(), err)
	}
}

func TestGRPCInvalidOrExpiredBudget(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		code string
	}{{"", "3"}, {"-1S", "3"}, {"1x", "3"}, {"123456789S", "3"}, {"0n", "4"}} {
		t.Run(tc.raw, func(t *testing.T) {
			cfg := config.Defaults()
			h := NewHandler(Options{Config: &cfg})
			req := httptest.NewRequest(http.MethodPost, "/service/Method", nil)
			req.ProtoMajor = 2
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Grpc-Timeout", tc.raw)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Header().Get("Grpc-Status") != tc.code {
				t.Fatalf("status = %v", w.Header())
			}
		})
	}
}

func TestGRPCPreservesMethodUnderBrowserPrefix(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.PathRoutingPrefix = "/grpc.testing.TestService"
	client := grpcRouterClient(t, Options{Config: &cfg})
	_, err := client.UnaryCall(grpcTargetContext(t, host, port), &grpc_testing.SimpleRequest{})
	if err != nil {
		t.Fatalf("native method must bypass browser prefix: %v", err)
	}
}

func TestGRPCBrokenStreamNeverBecomesOK(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint("connection interrupted=", interrupted), func(t *testing.T) {
			var operations atomic.Int32
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				operations.Add(1)
				payload, err := proto.Marshal(&grpc_testing.StreamingOutputCallResponse{Payload: &grpc_testing.Payload{Body: []byte("before-break")}})
				if err != nil {
					panic(err)
				}
				frame := make([]byte, 5+len(payload))
				binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
				copy(frame[5:], payload)
				w.Header().Set("Content-Type", "application/grpc")
				_, _ = w.Write(frame)
				w.(http.Flusher).Flush()
				if interrupted {
					<-r.Context().Done()
				}
				// Otherwise end normally at HTTP level, but without grpc-status.
			}))
			backend.Config.Protocols = new(http.Protocols)
			backend.Config.Protocols.SetUnencryptedHTTP2(true)
			backend.Start()
			t.Cleanup(backend.Close)
			host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			client := grpcRouterClient(t, Options{})
			stream, err := client.StreamingOutputCall(grpcTargetContext(t, host, port), &grpc_testing.StreamingOutputCallRequest{})
			if err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if err != nil || string(first.GetPayload().GetBody()) != "before-break" {
				t.Fatalf("first event: %v, %v", first, err)
			}
			if interrupted {
				backend.CloseClientConnections()
			}
			_, err = stream.Recv()
			if err == nil || errors.Is(err, io.EOF) || status.Code(err) == codes.OK {
				t.Fatalf("incomplete stream became OK: %v", err)
			}
			if operations.Load() != 1 {
				t.Fatalf("broken stream replayed %d times", operations.Load())
			}
		})
	}
}

func TestGRPCRecognition(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		protocol    int
		grpc        bool
	}{
		{"application/grpc", 2, true}, {"application/grpc+proto", 2, true},
		{"application/grpc; charset=utf-8", 2, true}, {"application/grpc-web", 2, false},
		{"application/grpc+", 2, false}, {"application/grpc", 1, false},
	} {
		t.Run(tc.contentType+fmt.Sprint(tc.protocol), func(t *testing.T) {
			cfg := config.Defaults()
			h := NewHandler(Options{Config: &cfg})
			t.Cleanup(h.CloseIdleConnections)
			r := httptest.NewRequest(http.MethodPost, "/service/Method", nil)
			r.ProtoMajor = tc.protocol
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Header().Get("Grpc-Status") != ""; got != tc.grpc {
				t.Fatalf("native=%v, want %v; headers=%v", got, tc.grpc, w.Header())
			}
		})
	}
}
