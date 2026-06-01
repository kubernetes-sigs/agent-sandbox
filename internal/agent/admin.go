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

package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

type AdminServer struct {
	pb.UnimplementedAdminServiceServer
	workspaceDir string
}

func NewAdminServer() *AdminServer {
	workspace := os.Getenv("SANDBOX_WORKSPACE")
	if workspace == "" {
		workspace = "/workspace"
	}
	return &AdminServer{
		workspaceDir: workspace,
	}
}

// Setup downloads a golden workspace tarball and extracts it to the local workspace directory
func (s *AdminServer) Setup(ctx context.Context, req *pb.SetupRequest) (*emptypb.Empty, error) {
	if req.RootfsUrl == "" {
		return nil, status.Error(codes.InvalidArgument, "rootfs_url cannot be empty")
	}

	// Ensure clean workspace first
	if err := s.clearWorkspace(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to clear workspace before setup: %v", err)
	}

	// Download tarball
	resp, err := http.Get(req.RootfsUrl)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to download rootfs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "failed to download rootfs, HTTP status: %d", resp.StatusCode)
	}

	// Extract tarball
	if err := s.extractTarGz(resp.Body); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to extract rootfs: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// Clean forcefully terminates running background user processes and clears the workspace
func (s *AdminServer) Clean(ctx context.Context, req *pb.CleanRequest) (*emptypb.Empty, error) {
	// 1. Wreak havoc on user-spawned processes
	if err := s.killUserProcesses(); err != nil {
		// We log or warn but continue to filesystem cleanup
	}

	// 2. Wipe the local workspace filesystem clean
	if err := s.clearWorkspace(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to clear workspace: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// clearWorkspace deletes all contents inside the configured workspace directory
func (s *AdminServer) clearWorkspace() error {
	if err := os.MkdirAll(s.workspaceDir, 0755); err != nil {
		return err
	}

	dir, err := os.ReadDir(s.workspaceDir)
	if err != nil {
		return err
	}

	for _, entry := range dir {
		path := filepath.Join(s.workspaceDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}

// extractTarGz uncompresses and extracts a tar.gz stream to our workspaceDir
func (s *AdminServer) extractTarGz(gzipStream io.Reader) error {
	uncompressedStream, err := gzip.NewReader(gzipStream)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer uncompressedStream.Close()

	tarReader := tar.NewReader(uncompressedStream)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return fmt.Errorf("tar reader error: %w", err)
		}

		target := filepath.Join(s.workspaceDir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create dir %s: %w", target, err)
			}
		case tar.TypeReg:
			baseDir := filepath.Dir(target)
			if err := os.MkdirAll(baseDir, 0755); err != nil {
				return fmt.Errorf("failed to create base dir %s: %w", baseDir, err)
			}
			
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", target, err)
			}
			
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to write file %s: %w", target, err)
			}
			outFile.Close()
		}
	}

	return nil
}

// killUserProcesses terminates all running PIDs in the container except PID 1 and the current process
func (s *AdminServer) killUserProcesses() error {
	myPid := os.Getpid()

	files, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("failed to read /proc: %w", err)
	}

	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(f.Name())
		if err != nil {
			continue // Not a PID folder
		}

		// Skip protected PIDs (PID 1 init, and current agent PID)
		if pid == 1 || pid == myPid {
			continue
		}

		// Get command name to avoid killing crucial infrastructure if any
		commBytes, err := os.ReadFile(filepath.Join("/proc", f.Name(), "comm"))
		if err == nil {
			comm := strings.TrimSpace(string(commBytes))
			if comm == "systemd" || comm == "grpc" || comm == "containerd" {
				continue
			}
		}

		// Force SIGKILL
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	}

	// If jupyter server is running, the clean operation will kill its processes as well.
	// The jupyter session mappings should be reset in the jupyter server instance.
	// We'll handle jupyter session cleanup by resetting it in the main orchestrator.
	return nil
}
