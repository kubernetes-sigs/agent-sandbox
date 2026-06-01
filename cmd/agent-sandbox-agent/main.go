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
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
	"sigs.k8s.io/agent-sandbox/internal/agent"
)

var (
	port = flag.Int("port", 50051, "The gRPC port to listen on")
)

func main() {
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen on port %d: %v", *port, err)
	}

	grpcServer := grpc.NewServer()

	// 1. Instantiate our modular servers
	processSrv := agent.NewProcessServer()
	filesystemSrv := agent.NewFilesystemServer()
	jupyterSrv := agent.NewJupyterServer()
	adminSrv := agent.NewAdminServer()

	// 2. Register services with gRPC
	pb.RegisterProcessServiceServer(grpcServer, processSrv)
	pb.RegisterFilesystemServiceServer(grpcServer, filesystemSrv)
	pb.RegisterJupyterServiceServer(grpcServer, jupyterSrv)
	pb.RegisterAdminServiceServer(grpcServer, adminSrv)

	// Enable gRPC reflection for easy testing/discovery via grpcurl
	reflection.Register(grpcServer)

	// 3. Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("received signal %v, initiating graceful shutdown...", sig)
		
		// Stop background Jupyter Server
		jupyterSrv.Stop()
		
		grpcServer.GracefulStop()
		log.Println("grpc server gracefully stopped.")
	}()

	log.Printf("Agent Sandbox Daemon listening at %v", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve gRPC: %v", err)
	}
}
