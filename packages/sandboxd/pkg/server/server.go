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
	"syscall"
	"time"

	"google.golang.org/grpc"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	filesystemv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/filesystem/v1"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type Server struct {
	rootDir          string
	registry         *processmanager.ProcessRegistry
	filesystemServer *FilesystemServer
	processServer    *ProcessServer
}

func New(rootDir string) *Server {
	if rootDir == "" {
		rootDir = "/"
	}
	registry := processmanager.NewProcessRegistry()
	return &Server{
		rootDir:          rootDir,
		registry:         registry,
		filesystemServer: NewFilesystemServer(rootDir),
		processServer:    NewProcessServer(rootDir, registry),
	}
}

func (s *Server) Register(grpcServer *grpc.Server) {
	filesystemv1.RegisterFilesystemServiceServer(grpcServer, s.filesystemServer)
	processv1.RegisterProcessServiceServer(grpcServer, s.processServer)
}

func (s *Server) Shutdown(ctx context.Context) {
	// Signal SIGTERM to all active processes
	s.registry.SignalAll(syscall.SIGTERM)

	// Wait grace period for processes to exit
	select {
	case <-time.After(2 * time.Second):
		// Send SIGKILL to remaining processes
		s.registry.SignalAll(syscall.SIGKILL)
	case <-ctx.Done():
		s.registry.SignalAll(syscall.SIGKILL)
	}
}
