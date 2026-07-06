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
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	filesystemv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/filesystem/v1"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func TestSandboxdIntegrationOverUDS(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "sandboxd.sock")
	rootDir := filepath.Join(tempDir, "root")

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		t.Fatalf("failed to create rootDir: %v", err)
	}

	// 1. Start gRPC Server over Unix Socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	srv := New(rootDir)
	srv.Register(grpcServer)

	go func() {
		_ = grpcServer.Serve(listener)
	}()
	defer grpcServer.Stop()

	// 2. Dial gRPC Server over UDS
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}

	conn, err := grpc.NewClient("passthrough:///unix",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial unix socket: %v", err)
	}
	defer conn.Close()

	fsClient := filesystemv1.NewFilesystemServiceClient(conn)
	procClient := processv1.NewProcessServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 3. Test Filesystem Operations
	makeDirRes, err := fsClient.MakeDir(ctx, &filesystemv1.MakeDirRequest{Path: "workspace"})
	if err != nil || makeDirRes == nil {
		t.Fatalf("MakeDir failed over UDS: %v", err)
	}

	statRes, err := fsClient.Stat(ctx, &filesystemv1.StatRequest{Path: "workspace"})
	if err != nil || statRes.GetType() != filesystemv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("Stat failed over UDS: %v", err)
	}

	// 4. Test Process Operations
	execRes, err := procClient.Execute(ctx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"echo", "sandboxd integration test passed"},
			Cwd:     proto.String("workspace"),
		},
	})
	if err != nil {
		t.Fatalf("Execute failed over UDS: %v", err)
	}

	if execRes.GetExitCode() != 0 {
		t.Errorf("expected exit code 0, got %d", execRes.GetExitCode())
	}

	if string(execRes.GetStdout()) != "sandboxd integration test passed\n" {
		t.Errorf("unexpected stdout: %q", string(execRes.GetStdout()))
	}
}
