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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/protobuf/types/known/timestamppb"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
	filesystemv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/filesystem/v1"
)

const (
	MaxListEntries = 10000
	ReadChunkSize  = 64 * 1024 // 64KB
)

type FilesystemServer struct {
	filesystemv1.UnimplementedFilesystemServiceServer
	rootDir string
}

func NewFilesystemServer(rootDir string) *FilesystemServer {
	if rootDir == "" {
		rootDir = "/"
	}
	return &FilesystemServer{rootDir: rootDir}
}

func (s *FilesystemServer) Write(stream filesystemv1.FilesystemService_WriteServer) (err error) {
	// Receive first frame which must contain metadata
	firstFrame, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive metadata frame: %w", err)
	}

	metadata := firstFrame.GetMetadata()
	if metadata == nil || metadata.Path == "" {
		return fmt.Errorf("first WriteRequest payload must be FileMetadata with non-empty path")
	}

	finalPath, err := pathutil.SanitizePath(s.rootDir, metadata.Path)
	if err != nil {
		return err
	}

	mode := os.FileMode(0644)
	if metadata.Mode != nil {
		mode = os.FileMode(metadata.GetMode())
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	// Create a temporary file in the same directory for atomic write
	tmpFile, err := os.CreateTemp(filepath.Dir(finalPath), ".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if err := os.Chmod(tmpFile.Name(), mode); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("failed to set temp file mode: %w", err)
	}

	// Cleanup temp file on error or context cancellation
	defer func() {
		_ = tmpFile.Close()
		if err != nil || stream.Context().Err() != nil {
			_ = os.Remove(tmpFile.Name())
		}
	}()

	// Stream file chunks
	for {
		req, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			err = fmt.Errorf("stream read error: %w", recvErr)
			return err
		}

		chunk := req.GetChunk()
		if len(chunk) > 0 {
			if _, wErr := tmpFile.Write(chunk); wErr != nil {
				err = fmt.Errorf("failed to write chunk: %w", wErr)
				return err
			}
		}
	}

	if err = tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomically move temp file to final destination
	if err = os.Rename(tmpFile.Name(), finalPath); err != nil {
		return fmt.Errorf("failed to rename temp file to final destination: %w", err)
	}

	return stream.SendAndClose(&filesystemv1.WriteResponse{})
}

func (s *FilesystemServer) Read(req *filesystemv1.ReadRequest, stream filesystemv1.FilesystemService_ReadServer) error {
	targetPath, err := pathutil.SanitizePath(s.rootDir, req.GetPath())
	if err != nil {
		return err
	}

	file, err := os.Open(targetPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	buffer := make([]byte, ReadChunkSize)
	for {
		n, rErr := file.Read(buffer)
		if n > 0 {
			if sErr := stream.Send(&filesystemv1.ReadResponse{Chunk: buffer[:n]}); sErr != nil {
				return fmt.Errorf("failed to send chunk: %w", sErr)
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return fmt.Errorf("failed to read file: %w", rErr)
		}
	}

	return nil
}

func (s *FilesystemServer) List(ctx context.Context, req *filesystemv1.ListRequest) (*filesystemv1.ListResponse, error) {
	targetPath, err := pathutil.SanitizePath(s.rootDir, req.GetPath())
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read dir: %w", err)
	}

	truncated := false
	if len(entries) > MaxListEntries {
		entries = entries[:MaxListEntries]
		truncated = true
	}

	// Sort entries by name for deterministic ordering
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	pbEntries := make([]*filesystemv1.FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		fileType := filesystemv1.FileType_FILE_TYPE_FILE
		if entry.IsDir() {
			fileType = filesystemv1.FileType_FILE_TYPE_DIRECTORY
		} else if entry.Type()&os.ModeSymlink != 0 {
			fileType = filesystemv1.FileType_FILE_TYPE_SYMLINK
		}

		pbEntries = append(pbEntries, &filesystemv1.FileEntry{
			Name:    entry.Name(),
			Type:    fileType,
			Size:    info.Size(),
			ModTime: timestamppb.New(info.ModTime()),
		})
	}

	return &filesystemv1.ListResponse{
		Entries:   pbEntries,
		Truncated: truncated,
	}, nil
}

func (s *FilesystemServer) Stat(ctx context.Context, req *filesystemv1.StatRequest) (*filesystemv1.StatResponse, error) {
	targetPath, err := pathutil.SanitizePath(s.rootDir, req.GetPath())
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat path: %w", err)
	}

	// Note: Symlinks are resolved inside pathutil.SanitizePath,
	// so os.Stat will always see the target file/directory type.
	fileType := filesystemv1.FileType_FILE_TYPE_FILE
	if info.IsDir() {
		fileType = filesystemv1.FileType_FILE_TYPE_DIRECTORY
	}

	return &filesystemv1.StatResponse{
		Path:    req.GetPath(),
		Type:    fileType,
		Size:    info.Size(),
		ModTime: timestamppb.New(info.ModTime()),
	}, nil
}

func (s *FilesystemServer) MakeDir(ctx context.Context, req *filesystemv1.MakeDirRequest) (*filesystemv1.MakeDirResponse, error) {
	targetPath, err := pathutil.SanitizePath(s.rootDir, req.GetPath())
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(targetPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	return &filesystemv1.MakeDirResponse{}, nil
}

func (s *FilesystemServer) Remove(ctx context.Context, req *filesystemv1.RemoveRequest) (*filesystemv1.RemoveResponse, error) {
	targetPath, err := pathutil.SanitizePath(s.rootDir, req.GetPath())
	if err != nil {
		return nil, err
	}

	var errRemove error
	if req.GetRecursive() {
		errRemove = os.RemoveAll(targetPath)
	} else {
		errRemove = os.Remove(targetPath)
	}

	if errRemove != nil {
		return nil, fmt.Errorf("failed to remove path: %w", errRemove)
	}

	return &filesystemv1.RemoveResponse{}, nil
}
