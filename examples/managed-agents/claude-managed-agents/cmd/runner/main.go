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

// Command runner is the in-sandbox daemon baked into the cma-runner image.
// It receives CMA work items from the controller over HTTP and executes the
// session's tool calls inside the sandbox pod.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/ports"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/runner"
)

func main() {
	cfg := runner.RunnerConfig{}
	var grpcAddr string
	flag.StringVar(&cfg.Addr, "addr", runner.DefaultRunnerAddr, "address the runner HTTP server listens on")
	flag.StringVar(&grpcAddr, "grpc-placeholder-addr", fmt.Sprintf(":%d", ports.RunnerGRPCPlaceholder), "address of the idle TCP listener that keeps the SDK tunnel's gRPC port open")
	flag.Parse()

	// The Go SDK's pod tunnel forwards both sandboxd data-plane ports (REST
	// and gRPC) when the controller opens a sandbox, and the SDK requires the
	// two ports to differ. The controller points the gRPC port at this idle
	// listener; it only needs to accept connections so the tunnel can
	// establish. The runner never speaks gRPC.
	go holdTCPListener(grpcAddr)

	r, err := runner.NewRunner(cfg)
	if err != nil {
		log.Fatalf("creating runner: %v", err)
	}
	log.Printf("runner listening on %s", cfg.Addr)
	if err := r.Run(); err != nil {
		log.Fatalf("runner exited: %v", err)
	}
}

// holdTCPListener accepts connections and holds them open until the process
// exits.
func holdTCPListener(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("gRPC placeholder listener on %s failed: %v", addr, err)
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}(conn)
	}
}
