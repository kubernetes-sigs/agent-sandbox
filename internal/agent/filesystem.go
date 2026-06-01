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
	"context"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

type FilesystemServer struct {
	pb.UnimplementedFilesystemServiceServer
}

func NewFilesystemServer() *FilesystemServer {
	return &FilesystemServer{}
}

// WriteFile uploads or writes a file's binary content to a specific path
func (s *FilesystemServer) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*emptypb.Empty, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	// Ensure target directory exists
	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create directory structure: %v", err)
	}

	// Write file contents
	if err := os.WriteFile(req.Path, req.Content, 0644); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to write file: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// ReadFile downloads or reads a file's binary content from a specific path
func (s *FilesystemServer) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	content, err := os.ReadFile(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "file %s not found", req.Path)
		}
		return nil, status.Errorf(codes.Internal, "failed to read file: %v", err)
	}

	return &pb.ReadFileResponse{
		Content: content,
	}, nil
}

// ListFiles lists directory contents with file sizes and metadata
func (s *FilesystemServer) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	entries, err := os.ReadDir(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "directory %s not found", req.Path)
		}
		return nil, status.Errorf(codes.Internal, "failed to read directory: %v", err)
	}

	var fileEntries []*pb.FileEntry
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch info for %s: %v", entry.Name(), err)
		}

		fileEntries = append(fileEntries, &pb.FileEntry{
			Name:    entry.Name(),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: timestamppb.New(info.ModTime()),
		})
	}

	return &pb.ListFilesResponse{
		Entries: fileEntries,
	}, nil
}

// StatFile retrieves detailed metadata for a single file or directory
func (s *FilesystemServer) StatFile(ctx context.Context, req *pb.StatFileRequest) (*pb.StatFileResponse, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	info, err := os.Stat(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "file or directory %s not found", req.Path)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat path: %v", err)
	}

	return &pb.StatFileResponse{
		Path:    req.Path,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: timestamppb.New(info.ModTime()),
	}, nil
}

// MakeDir creates a directory (and parent directories if missing)
func (s *FilesystemServer) MakeDir(ctx context.Context, req *pb.MakeDirRequest) (*emptypb.Empty, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	if err := os.MkdirAll(req.Path, 0755); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create directory: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// Remove deletes a file or directory (with optional recursive deletion)
func (s *FilesystemServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*emptypb.Empty, error) {
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path cannot be empty")
	}

	var err error
	if req.Recursive {
		err = os.RemoveAll(req.Path)
	} else {
		err = os.Remove(req.Path)
	}

	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "file or directory %s not found", req.Path)
		}
		return nil, status.Errorf(codes.Internal, "failed to delete path: %v", err)
	}

	return &emptypb.Empty{}, nil
}
