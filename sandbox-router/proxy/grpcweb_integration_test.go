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

//go:build integration

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// grpcWebRequest builds a hand-rolled binary gRPC-Web POST — exactly what
// a browser fetch produces — so the tests exercise the real wire format
// without a JS client. The default client speaks HTTP/1.1, which is the
// regression lock for the h1-inbound → h2c-outbound path.
func grpcWebRequest(t *testing.T, url, contentType string, msg proto.Message, headers map[string]string) *http.Request {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("X-User-Agent", "grpc-web-javascript/0.1")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// parseGRPCWebBody splits a grpc-web response body into message payloads
// and the terminal trailer text.
func parseGRPCWebBody(t *testing.T, body []byte) (messages [][]byte, trailerText string) {
	t.Helper()
	for len(body) > 0 {
		if len(body) < 5 {
			t.Fatalf("truncated frame header: %d bytes left", len(body))
		}
		flag := body[0]
		length := binary.BigEndian.Uint32(body[1:5])
		if uint32(len(body)-5) < length {
			t.Fatalf("truncated frame payload: have %d, want %d", len(body)-5, length)
		}
		payload := body[5 : 5+length]
		if flag&grpcWebTrailerFlag != 0 {
			if trailerText != "" {
				t.Fatal("more than one trailer frame")
			}
			trailerText = string(payload)
		} else {
			messages = append(messages, payload)
		}
		body = body[5+length:]
	}
	return messages, trailerText
}

func grpcWebRouterServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	if opts.Config == nil {
		opts.Config = routerConfig()
	}
	opts.Logger = logr.Discard()
	handler := NewHandler(opts)
	t.Cleanup(handler.Close)
	// HTTP/1 only on the listener: browser fetch over TLS negotiates h1
	// or h2, and the h1 path is the one nothing else in the suite covers.
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func routerConfig() *config.Config {
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.UpstreamMaxRetries = 0
	cfg.GRPCWebAllowedOrigins = []string{"https://app.example.com"}
	return &cfg
}

func grpcWebRoutingHeaders(host, port string) map[string]string {
	return map[string]string{
		"X-Sandbox-ID":     "sb1",
		"X-Sandbox-Port":   port,
		"X-Sandbox-Pod-IP": host,
	}
}

func TestGRPCWebUnaryOverHTTP1Inbound(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	router := grpcWebRouterServer(t, Options{})

	req := grpcWebRequest(t, router.URL+"/grpc.testing.TestService/UnaryCall",
		contentTypeGRPCWeb,
		&grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte("hello")}},
		grpcWebRoutingHeaders(host, port))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 1 {
		t.Fatalf("inbound response proto = HTTP/%d, test must exercise HTTP/1.1", resp.ProtoMajor)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentTypeGRPCWeb {
		t.Fatalf("content-type = %q, want %q", ct, contentTypeGRPCWeb)
	}
	body, _ := io.ReadAll(resp.Body)
	messages, trailerText := parseGRPCWebBody(t, body)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	var out grpc_testing.SimpleResponse
	if err := proto.Unmarshal(messages[0], &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(out.GetPayload().GetBody()) != "hello" {
		t.Fatalf("echo = %q, want hello", out.GetPayload().GetBody())
	}
	if !strings.Contains(trailerText, "grpc-status:0\r\n") {
		t.Fatalf("trailer = %q, want grpc-status:0", trailerText)
	}
}

func TestGRPCWebUnaryOverH2CInbound(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})

	// The default httptest listener speaks HTTP/1.1 only; browsers may
	// also arrive over h2c on the plain listener, which the real server
	// enables via Protocols. Mirror that here.
	h2cRouter := httptest.NewUnstartedServer(NewHandler(Options{
		Config: routerConfig(),
		Logger: logr.Discard(),
	}))
	h2cRouter.Config.Protocols = new(http.Protocols)
	h2cRouter.Config.Protocols.SetHTTP1(true)
	h2cRouter.Config.Protocols.SetUnencryptedHTTP2(true)
	h2cRouter.Start()
	defer h2cRouter.Close()

	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	req := grpcWebRequest(t, h2cRouter.URL+"/grpc.testing.TestService/UnaryCall", contentTypeGRPCWeb,
		&grpc_testing.SimpleRequest{Payload: &grpc_testing.Payload{Body: []byte("h2c")}},
		grpcWebRoutingHeaders(host, port))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("inbound response proto = HTTP/%d, want HTTP/2", resp.ProtoMajor)
	}
	body, _ := io.ReadAll(resp.Body)
	_, trailerText := parseGRPCWebBody(t, body)
	if !strings.Contains(trailerText, "grpc-status:0\r\n") {
		t.Fatalf("trailer = %q, want grpc-status:0", trailerText)
	}
}

func TestGRPCWebServerStreaming(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{
		output: func(_ *grpc_testing.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[grpc_testing.StreamingOutputCallResponse]) error {
			for _, body := range []string{"a", "b", "c"} {
				if err := stream.Send(&grpc_testing.StreamingOutputCallResponse{
					Payload: &grpc_testing.Payload{Body: []byte(body)},
				}); err != nil {
					return err
				}
			}
			return nil
		},
	})
	router := grpcWebRouterServer(t, Options{})

	req := grpcWebRequest(t, router.URL+"/grpc.testing.TestService/StreamingOutputCall",
		contentTypeGRPCWeb, &grpc_testing.StreamingOutputCallRequest{}, grpcWebRoutingHeaders(host, port))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	messages, trailerText := parseGRPCWebBody(t, body)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(messages))
	}
	for i, want := range []string{"a", "b", "c"} {
		var out grpc_testing.StreamingOutputCallResponse
		if err := proto.Unmarshal(messages[i], &out); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
		if string(out.GetPayload().GetBody()) != want {
			t.Fatalf("message %d = %q, want %q", i, out.GetPayload().GetBody(), want)
		}
	}
	if !strings.Contains(trailerText, "grpc-status:0\r\n") {
		t.Fatalf("trailer = %q, want grpc-status:0", trailerText)
	}
}

func TestGRPCWebTrailersOnlyBackendError(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{
		unary: func(context.Context, *grpc_testing.SimpleRequest) (*grpc_testing.SimpleResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "denied by backend")
		},
	})
	router := grpcWebRouterServer(t, Options{})

	req := grpcWebRequest(t, router.URL+"/grpc.testing.TestService/UnaryCall",
		contentTypeGRPCWeb, &grpc_testing.SimpleRequest{}, grpcWebRoutingHeaders(host, port))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	messages, trailerText := parseGRPCWebBody(t, body)
	if len(messages) != 0 {
		t.Fatalf("messages = %d, want 0 for trailers-only", len(messages))
	}
	if !strings.Contains(trailerText, "grpc-status:7\r\n") {
		t.Fatalf("trailer = %q, want grpc-status:7", trailerText)
	}
	// The trailer frame carries grpc-message exactly as the backend put
	// it on the wire — grpc-go sent a raw space here, and re-encoding
	// would corrupt messages that are already percent-encoded.
	if !strings.Contains(trailerText, "grpc-message:denied by backend\r\n") {
		t.Fatalf("trailer = %q, want the backend message preserved verbatim", trailerText)
	}
}

func TestGRPCWebRouterErrors(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	deadPort := pickFreePortStr(t)
	router := grpcWebRouterServer(t, Options{})

	for _, tc := range []struct {
		name        string
		path        string
		headers     map[string]string
		contentType string
		wantStatus  string
	}{
		{
			name:        "missing sandbox id",
			path:        "/grpc.testing.TestService/UnaryCall",
			headers:     map[string]string{},
			contentType: contentTypeGRPCWeb,
			wantStatus:  "grpc-status:3\r\n", // InvalidArgument
		},
		{
			name:        "unreachable backend",
			path:        "/grpc.testing.TestService/UnaryCall",
			headers:     grpcWebRoutingHeaders(host, deadPort),
			contentType: contentTypeGRPCWeb,
			wantStatus:  "grpc-status:14\r\n", // Unavailable
		},
		{
			name:        "text variant rejected",
			path:        "/grpc.testing.TestService/UnaryCall",
			headers:     grpcWebRoutingHeaders(host, port),
			contentType: "application/grpc-web-text",
			wantStatus:  "grpc-status:3\r\n", // InvalidArgument
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := grpcWebRequest(t, router.URL+tc.path, tc.contentType, &grpc_testing.SimpleRequest{}, tc.headers)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("http status = %d, want 200 with the outcome in the body", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != contentTypeGRPCWeb {
				t.Fatalf("content-type = %q", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			messages, trailerText := parseGRPCWebBody(t, body)
			if len(messages) != 0 {
				t.Fatalf("messages = %d, want 0", len(messages))
			}
			if !strings.Contains(trailerText, tc.wantStatus) {
				t.Fatalf("trailer = %q, want %q", trailerText, tc.wantStatus)
			}
		})
	}
}

func TestGRPCWebPreflightThroughRealListener(t *testing.T) {
	router := grpcWebRouterServer(t, Options{})

	req, err := http.NewRequest(http.MethodOptions, router.URL+"/grpc.testing.TestService/UnaryCall", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type,x-sandbox-id,x-sandbox-port")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q", got)
	}
}

func TestGRPCWebCORSHeadersOnRealResponse(t *testing.T) {
	host, port := grpcBackend(t, &grpcEchoService{})
	router := grpcWebRouterServer(t, Options{})

	req := grpcWebRequest(t, router.URL+"/grpc.testing.TestService/UnaryCall",
		contentTypeGRPCWeb, &grpc_testing.SimpleRequest{}, grpcWebRoutingHeaders(host, port))
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO = %q, want echoed origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC = %q, want true", got)
	}
}
