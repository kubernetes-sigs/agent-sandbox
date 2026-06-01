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

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

func main() {
	// Connect to local port forwarded sandbox daemon
	conn, err := grpc.Dial("127.0.0.1:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fsClient := pb.NewFilesystemServiceClient(conn)
	procClient := pb.NewProcessServiceClient(conn)
	jupyterClient := pb.NewJupyterServiceClient(conn)
	adminClient := pb.NewAdminServiceClient(conn)

	fmt.Println("=== 1. Writing File ===")
	// We write a Python script instead of a Go script to ensure compatibility
	_, err = fsClient.WriteFile(ctx, &pb.WriteFileRequest{
		Path:    "/tmp/hello.py",
		Content: []byte("message = \"Hello from Python inside GKE Sandbox!\"\nprint(message)"),
	})
	if err != nil {
		log.Fatalf("failed to write file: %v", err)
	}
	fmt.Println("Successfully wrote /tmp/hello.py inside sandbox")

	fmt.Println("\n=== 2. Executing Command ===")
	// Execute the Python script
	execResp, err := procClient.Execute(ctx, &pb.ExecuteRequest{
		Command: []string{"python3", "/tmp/hello.py"},
	})
	if err != nil {
		log.Fatalf("failed to execute: %v", err)
	}
	fmt.Printf("Exit Code: %d\n", execResp.ExitCode)
	fmt.Printf("Stdout: %s\n", execResp.Stdout)

	fmt.Println("\n=== 3. Creating Jupyter Session ===")
	session, err := jupyterClient.CreateSession(ctx, &pb.CreateJupyterSessionRequest{
		KernelName: "python3",
	})
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	fmt.Printf("Session ID: %s\n", session.SessionId)

	res, err := jupyterClient.ExecuteCode(ctx, &pb.ExecuteJupyterCodeRequest{
		SessionId: session.SessionId,
		Code:      "a = 25\nb = 4\nprint(a * b)",
	})
	if err != nil {
		log.Fatalf("failed to run code: %v", err)
	}
	fmt.Printf("Jupyter Output: %s\n", res.Stdout)

	fmt.Println("\n=== 4. Wiping Workspace ===")
	_, err = adminClient.Clean(ctx, &pb.CleanRequest{})
	if err != nil {
		log.Fatalf("failed to clean: %v", err)
	}
	fmt.Println("Sandbox wiped clean successfully.")
}
