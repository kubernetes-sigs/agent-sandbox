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
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

const testToken = "s3cret-token"

func digestOf(token string) []byte {
	d := sha256.Sum256([]byte(token))
	return d[:]
}

func newAuthServer(t *testing.T, token string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	var digest []byte
	if token != "" {
		digest = digestOf(token)
	}
	srv, err := New(Options{RootDir: root, AuthTokenSHA256: digest, Log: logr.Discard()})
	require.NoError(t, err)
	return srv, root
}

func newAuthGRPCClient(t *testing.T, srv *Server) processv1.ProcessServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(srv.GRPCServerOptions()...)
	srv.RegisterGRPC(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		grpcServer.Stop()
	})
	return processv1.NewProcessServiceClient(conn)
}

func withAuth(ctx context.Context, value string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", value)
}

func TestRESTRequiresBearerToken(t *testing.T) {
	srv, root := newAuthServer(t, testToken)
	handler := srv.RESTHandler()
	put := func(auth string) int {
		h := http.Header{}
		if auth != "" {
			h.Set("Authorization", auth)
		}
		return doRequest(handler, http.MethodPut, "/v1/files/a.txt", []byte("x"), h).Code
	}

	for _, auth := range []string{"", "Bearer wrong", "Basic " + testToken, testToken, "Bearer"} {
		require.Equal(t, http.StatusUnauthorized, put(auth), "auth %q", auth)
	}
	_, err := os.Stat(filepath.Join(root, "a.txt"))
	require.True(t, os.IsNotExist(err), "a refused PUT must not write")

	rec := doRequest(handler, http.MethodGet, "/v1/files/a.txt", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "UNAUTHENTICATED", decodeError(t, rec).Code)
	require.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")

	require.Equal(t, http.StatusNoContent, put("Bearer "+testToken))
	require.Equal(t, http.StatusNoContent, put("bearer "+testToken))
}

func TestRESTHealthOpenWithoutToken(t *testing.T) {
	srv, _ := newAuthServer(t, testToken)
	rec := doRequest(srv.RESTHandler(), http.MethodGet, "/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(srv.RESTHandler(), http.MethodGet, "/v1/metadata", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGRPCRequiresBearerToken(t *testing.T) {
	srv, _ := newAuthServer(t, testToken)
	client := newAuthGRPCClient(t, srv)
	req := &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"true"}}}

	for _, ctx := range []context.Context{
		testCtx(t),
		withAuth(testCtx(t), "Bearer wrong"),
		withAuth(testCtx(t), testToken),
	} {
		_, err := client.Execute(ctx, req)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	}

	stream, err := client.Start(testCtx(t), &processv1.StartRequest{Config: req.GetConfig()})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	resp, err := client.Execute(withAuth(testCtx(t), "Bearer "+testToken), req)
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.GetExitCode())

	stream, err = client.Start(withAuth(testCtx(t), "Bearer "+testToken),
		&processv1.StartRequest{Config: req.GetConfig()})
	require.NoError(t, err)
	for {
		ev, err := stream.Recv()
		require.NoError(t, err)
		if ev.GetExit() != nil {
			require.Equal(t, int32(0), ev.GetExit().GetExitCode())
			break
		}
	}
}

func TestNoTokenLeavesAPIOpen(t *testing.T) {
	srv, _ := newAuthServer(t, "")
	require.Empty(t, srv.GRPCServerOptions())
	rec := doRequest(srv.RESTHandler(), http.MethodPut, "/v1/files/a.txt", []byte("x"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestParseAuthTokenSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte(testToken))
	got, err := ParseAuthTokenSHA256(hex.EncodeToString(sum[:]) + "\n")
	require.NoError(t, err)
	require.Equal(t, sum[:], got)

	for _, bad := range []string{"", "zz", hex.EncodeToString(sum[:16])} {
		_, err := ParseAuthTokenSHA256(bad)
		require.Error(t, err, "digest %q", bad)
	}
}

func TestNewRejectsShortDigest(t *testing.T) {
	_, err := New(Options{RootDir: t.TempDir(), AuthTokenSHA256: []byte("short"), Log: logr.Discard()})
	require.Error(t, err)
}

func TestLoadAuthTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte(testToken+"\n"), 0o600))
	got, err := LoadAuthTokenFile(path)
	require.NoError(t, err)
	require.Equal(t, digestOf(testToken), got)

	require.NoError(t, os.WriteFile(path, []byte(" \n"), 0o600))
	_, err = LoadAuthTokenFile(path)
	require.Error(t, err)

	_, err = LoadAuthTokenFile(filepath.Join(dir, "missing"))
	require.Error(t, err)
}
