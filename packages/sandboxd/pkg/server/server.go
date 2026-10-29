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
// KEP-539.2: the ProcessService gRPC API (real-time process execution),
// the Filesystem & Runtime REST API (stateless file transfers and probes),
// and the FileWatcherService gRPC API (real-time filesystem event streaming).
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
)

// Process-lifecycle timing knobs for the server package. Daemon-level knobs
// (HTTP timeouts, shutdown budget) live in cmd/sandboxd/main.go, mirroring
// how envd organizes its constants.
const (
	// processShutdownGrace is how long managed processes get to exit after
	// SIGTERM before being SIGKILLed during daemon shutdown.
	processShutdownGrace = 2 * time.Second
	// pipeDrainGrace bounds how long Start waits for the stdout/stderr pipe
	// readers, or the PTY reader, to reach EOF after cmd.Wait has reaped the
	// child process (a grandchild may still hold the write end or the PTY
	// slave).
	pipeDrainGrace = 5 * time.Second
	// waitDelay bounds how long cmd.Wait blocks on I/O drain after the
	// child exits. Without it, a grandchild holding the stdout/stderr
	// write ends keeps Wait blocked indefinitely.
	waitDelay = 10 * time.Second
	// executionCleanupTimeout bounds waiting for unary termination and reaping.
	executionCleanupTimeout = 10 * time.Second
)

// Options configures a sandboxd Server.
type Options struct {
	// RootDir is the sandbox root all file operations and working
	// directories are confined to. Required: New returns an error when
	// empty rather than defaulting to "/", which would disable confinement.
	RootDir string
	// MetadataEnvPrefix selects which environment variables are exposed on
	// GET /v1/metadata.
	MetadataEnvPrefix string
	// StreamChunkSize is the stdout/stderr streaming buffer size in bytes
	// for ProcessService.Start. Non-positive selects the 4096-byte default.
	StreamChunkSize int
	// ExecutionPolicy applies defaults and ceilings to unary command execution.
	ExecutionPolicy ExecutionPolicy
	// AuthTokenSHA256, when set, is the SHA-256 digest of a bearer token
	// required as "Authorization: Bearer <token>" on every gRPC call and
	// every REST request except GET /v1/health. Nil leaves the API
	// unauthenticated.
	AuthTokenSHA256 []byte
	Log             logr.Logger
}

// Server bundles the gRPC ProcessService and FileWatcherService and the REST
// FilesystemService behind a shared process registry so shutdown can reach
// every child process.
type Server struct {
	registry      *processmanager.ProcessRegistry
	processServer *ProcessServer
	watcherServer *FileWatcherServer
	restServer    *RESTServer
	auth          *tokenChecker
}

// New assembles a Server from opts.
func New(opts Options) (*Server, error) {
	if opts.RootDir == "" {
		return nil, errors.New("RootDir is required: refusing to default to \"/\", which would disable sandbox confinement")
	}
	if err := opts.ExecutionPolicy.validate(); err != nil {
		return nil, fmt.Errorf("invalid execution policy: %w", err)
	}
	registry := processmanager.NewProcessRegistry()
	s := &Server{
		registry:      registry,
		processServer: NewProcessServer(opts.RootDir, registry, opts.StreamChunkSize, opts.Log),
		watcherServer: NewFileWatcherServer(opts.RootDir, opts.Log),
		restServer:    NewRESTServer(opts.RootDir, opts.MetadataEnvPrefix, opts.Log),
	}
	s.processServer.executionPolicy = opts.ExecutionPolicy
	if opts.AuthTokenSHA256 != nil {
		if len(opts.AuthTokenSHA256) != AuthTokenDigestSize {
			return nil, fmt.Errorf("AuthTokenSHA256 must be %d bytes, got %d",
				AuthTokenDigestSize, len(opts.AuthTokenSHA256))
		}
		s.auth = newTokenChecker(opts.AuthTokenSHA256)
	}
	return s, nil
}

// GRPCServerOptions returns the options the gRPC server must be built with:
// the bearer-token interceptors when an auth token is configured. They apply
// to every service on that server, reflection included.
func (s *Server) GRPCServerOptions() []grpc.ServerOption {
	if s.auth == nil {
		return nil
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(s.auth.unaryInterceptor),
		grpc.ChainStreamInterceptor(s.auth.streamInterceptor),
	}
}

// RegisterGRPC attaches the ProcessService and FileWatcherService to grpcServer.
func (s *Server) RegisterGRPC(grpcServer *grpc.Server) {
	processv1.RegisterProcessServiceServer(grpcServer, s.processServer)
	watcherv1.RegisterFileWatcherServiceServer(grpcServer, s.watcherServer)
}

// RESTHandler returns the handler serving the Filesystem & Runtime REST API.
func (s *Server) RESTHandler() http.Handler {
	if s.auth == nil {
		return s.restServer.Handler()
	}
	return s.auth.middleware(s.restServer.Handler(), s.restServer)
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

// ShutdownWatches ends all active WatchDir streams with a clean
// end-of-stream, unblocking a subsequent grpc GracefulStop the same way
// ShutdownProcesses does for the Start streams.
func (s *Server) ShutdownWatches() {
	s.watcherServer.Shutdown()
}
