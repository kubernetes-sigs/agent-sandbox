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
	"flag"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
)

func main() {
	var (
		socketPath string
		rootDir    string
	)

	flag.StringVar(&socketPath, "socket-path", "/var/run/sandboxd/sandboxd.sock", "Path to Unix Domain Socket for sandboxd")
	flag.StringVar(&rootDir, "root-dir", "/", "Root directory path for sandbox file I/O")
	klog.InitFlags(nil)
	flag.Parse()

	klog.Infof("Starting sandboxd daemon (socket: %s, root-dir: %s)...", socketPath, rootDir)

	// Ensure socket directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		klog.Fatalf("Failed to create socket directory: %v", err)
	}

	// Pre-bind stale socket cleanup (handles abrupt crashes like OOM or SIGKILL)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Failed to remove existing socket file %s: %v", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		klog.Fatalf("Failed to listen on unix socket %s: %v", socketPath, err)
	}
	// Make socket writable by non-root users inside the pod
	_ = os.Chmod(socketPath, 0666)

	grpcServer := grpc.NewServer()
	srv := server.New(rootDir)
	srv.Register(grpcServer)

	// Catch termination signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)

	go func() {
		sig := <-sigChan
		klog.Infof("Received signal %v, initiating graceful shutdown...", sig)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		srv.Shutdown(shutdownCtx)
		grpcServer.GracefulStop()
		_ = listener.Close()
		_ = os.Remove(socketPath)

		klog.Info("sandboxd daemon stopped successfully.")
		os.Exit(0)
	}()

	klog.Infof("sandboxd daemon listening on %s", socketPath)
	if err := grpcServer.Serve(listener); err != nil && err != grpc.ErrServerStopped {
		klog.Fatalf("sandboxd server error: %v", err)
	}
}
