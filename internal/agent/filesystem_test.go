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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

func TestFilesystemOperations_FullFlow(t *testing.T) {
	server := NewFilesystemServer()
	ctx := context.Background()

	// Use a secure OS-provided temp directory for the test
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "nested", "test.txt")

	// 1. Write file
	writeReq := &pb.WriteFileRequest{
		Path:    filePath,
		Content: []byte("sandbox file contents"),
	}
	_, err := server.WriteFile(ctx, writeReq)
	assert.NoError(t, err)

	// 2. Stat file
	statReq := &pb.StatFileRequest{Path: filePath}
	statResp, err := server.StatFile(ctx, statReq)
	assert.NoError(t, err)
	assert.Equal(t, filePath, statResp.Path)
	assert.False(t, statResp.IsDir)
	assert.Equal(t, int64(len("sandbox file contents")), statResp.Size)

	// 3. Read file
	readReq := &pb.ReadFileRequest{Path: filePath}
	readResp, err := server.ReadFile(ctx, readReq)
	assert.NoError(t, err)
	assert.Equal(t, []byte("sandbox file contents"), readResp.Content)

	// 4. List directory
	listReq := &pb.ListFilesRequest{Path: filepath.Dir(filePath)}
	listResp, err := server.ListFiles(ctx, listReq)
	assert.NoError(t, err)
	assert.Len(t, listResp.Entries, 1)
	assert.Equal(t, "test.txt", listResp.Entries[0].Name)
	assert.False(t, listResp.Entries[0].IsDir)

	// 5. Remove file
	removeReq := &pb.RemoveRequest{
		Path:      filePath,
		Recursive: false,
	}
	_, err = server.Remove(ctx, removeReq)
	assert.NoError(t, err)

	// Verify file is gone
	_, err = server.ReadFile(ctx, readReq)
	assert.Error(t, err)
}

func TestMakeDir_And_RemoveDir(t *testing.T) {
	server := NewFilesystemServer()
	ctx := context.Background()

	tempDir := t.TempDir()
	dirPath := filepath.Join(tempDir, "custom-dir")

	// 1. Create directory
	_, err := server.MakeDir(ctx, &pb.MakeDirRequest{Path: dirPath})
	assert.NoError(t, err)

	// Verify dir exists via stat
	statResp, err := server.StatFile(ctx, &pb.StatFileRequest{Path: dirPath})
	assert.NoError(t, err)
	assert.True(t, statResp.IsDir)

	// 2. Remove directory recursively
	_, err = server.Remove(ctx, &pb.RemoveRequest{
		Path:      dirPath,
		Recursive: true,
	})
	assert.NoError(t, err)

	// Verify gone
	_, err = server.StatFile(ctx, &pb.StatFileRequest{Path: dirPath})
	assert.Error(t, err)
}
