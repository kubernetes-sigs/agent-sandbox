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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// TestSandboxdIntegration boots the full hybrid daemon — gRPC ProcessService
// and REST FilesystemService on ephemeral localhost ports — and drives a
// PUT → Execute → GET → DELETE round trip across both protocols.
func TestSandboxdIntegration(t *testing.T) {
	root := t.TempDir()
	srv := New(Options{RootDir: root, MetadataEnvPrefix: "SANDBOX_", Log: logr.Discard()})

	// gRPC ProcessService on an ephemeral localhost port.
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	srv.RegisterGRPC(grpcServer)
	grpcDone := make(chan struct{})
	go func() {
		defer close(grpcDone)
		_ = grpcServer.Serve(grpcLis)
	}()

	// REST FilesystemService on an ephemeral localhost port.
	restLis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpServer := &http.Server{Handler: srv.RESTHandler(), ReadHeaderTimeout: 10 * time.Second}
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		_ = httpServer.Serve(restLis)
	}()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, httpServer.Shutdown(shutdownCtx))
		grpcServer.GracefulStop()
		<-grpcDone
		<-httpDone
	})

	restBase := fmt.Sprintf("http://%s/v1", restLis.Addr())
	httpClient := &http.Client{Timeout: 10 * time.Second}

	conn, err := grpc.NewClient(grpcLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	processClient := processv1.NewProcessServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// 1. Health probe.
	resp, err := httpClient.Get(restBase + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// 2. PUT a file over REST.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		restBase+"/files/notes/hello.txt", bytes.NewReader([]byte("hello from REST")))
	require.NoError(t, err)
	resp, err = httpClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// 3. Read it back through the gRPC ProcessService.
	execResp, err := processClient.Execute(ctx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"cat", "notes/hello.txt"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), execResp.GetExitCode())
	require.Equal(t, "hello from REST", string(execResp.GetStdout()))

	// 4. GET it back over REST.
	resp, err = httpClient.Get(restBase + "/files/notes/hello.txt")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	gotBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "hello from REST", string(gotBody))

	// 5. Directory listing shows the file.
	resp, err = httpClient.Get(restBase + "/files/notes")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listing DirectoryListing
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listing))
	require.NoError(t, resp.Body.Close())
	require.Len(t, listing.Entries, 1)
	require.Equal(t, "hello.txt", listing.Entries[0].Name)

	// 6. DELETE over REST, confirm gone.
	req, err = http.NewRequestWithContext(ctx, http.MethodDelete, restBase+"/files/notes/hello.txt", nil)
	require.NoError(t, err)
	resp, err = httpClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp, err = httpClient.Get(restBase + "/files/notes/hello.txt")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// 7. Readiness flip mirrors graceful shutdown behavior.
	srv.SetReady(false)
	resp, err = httpClient.Get(restBase + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	srv.SetReady(true)
}

// TestShutdownProcessesTerminatesChildren verifies that daemon shutdown
// SIGTERMs managed processes so their Start streams end.
func TestShutdownProcessesTerminatesChildren(t *testing.T) {
	root := t.TempDir()
	srv := New(Options{RootDir: root, MetadataEnvPrefix: "SANDBOX_", Log: logr.Discard()})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	srv.RegisterGRPC(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	client := processv1.NewProcessServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sleep", "300"}},
	})
	require.NoError(t, err)
	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetInit())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.ShutdownProcesses(shutdownCtx)

	// The sleeper was signalled: the stream must end with a non-zero exit.
	sawExit := false
	for {
		msg, err := stream.Recv()
		if err != nil {
			break
		}
		if exit := msg.GetExit(); exit != nil {
			sawExit = true
			require.NotEqual(t, int32(0), exit.GetExitCode())
		}
	}
	require.True(t, sawExit, "expected an ExitEvent after shutdown")

	require.NoError(t, conn.Close())
	grpcServer.GracefulStop()
	_ = lis.Close()
}
