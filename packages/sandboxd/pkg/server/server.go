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

// Package server implements the sandboxd hybrid runtime API defined by
// KEP-539.2: the ProcessService gRPC API (real-time process execution) and
// the Filesystem & Runtime REST API (stateless file transfers and probes).
package server

import (
	"context"
	"net/http"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// processShutdownGrace is how long managed processes get to exit after
// SIGTERM before being SIGKILLed during daemon shutdown.
const processShutdownGrace = 2 * time.Second

// Options configures a sandboxd Server.
type Options struct {
	// RootDir is the sandbox root all file operations and working
	// directories are confined to. Defaults to "/" when empty.
	RootDir string
	// MetadataEnvPrefix selects which environment variables are exposed on
	// GET /v1/metadata.
	MetadataEnvPrefix string
	Log               logr.Logger
}

// Server bundles the gRPC ProcessService and REST FilesystemService behind a
// shared process registry so shutdown can reach every child process.
type Server struct {
	registry      *processmanager.ProcessRegistry
	processServer *ProcessServer
	restServer    *RESTServer
}

// New assembles a Server from opts.
func New(opts Options) *Server {
	rootDir := opts.RootDir
	if rootDir == "" {
		rootDir = "/"
	}
	registry := processmanager.NewProcessRegistry()
	return &Server{
		registry:      registry,
		processServer: NewProcessServer(rootDir, registry),
		restServer:    NewRESTServer(rootDir, opts.MetadataEnvPrefix, opts.Log),
	}
}

// RegisterGRPC attaches the ProcessService to grpcServer.
func (s *Server) RegisterGRPC(grpcServer *grpc.Server) {
	processv1.RegisterProcessServiceServer(grpcServer, s.processServer)
}

// RESTHandler returns the handler serving the Filesystem & Runtime REST API.
func (s *Server) RESTHandler() http.Handler {
	return s.restServer.Handler()
}

// SetReady toggles the /v1/health readiness response.
func (s *Server) SetReady(ready bool) {
	s.restServer.SetReady(ready)
}

// ShutdownProcesses terminates all managed processes: SIGTERM, a grace
// period, then SIGKILL for stragglers. Ending the processes also ends their
// Start streams, unblocking a subsequent grpc GracefulStop.
func (s *Server) ShutdownProcesses(ctx context.Context) {
	s.registry.SignalAll(syscall.SIGTERM)

	select {
	case <-time.After(processShutdownGrace):
	case <-ctx.Done():
	}
	s.registry.SignalAll(syscall.SIGKILL)
}
