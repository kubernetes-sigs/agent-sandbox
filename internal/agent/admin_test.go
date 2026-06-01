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
	"testing"

	"github.com/stretchr/testify/assert"
	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

func TestAdminClean_WipesWorkspace(t *testing.T) {
	tempWorkspace := t.TempDir()

	server := &AdminServer{
		workspaceDir: tempWorkspace,
	}
	ctx := context.Background()

	// 1. Populate workspace with test files and subdirs
	file1 := filepath.Join(tempWorkspace, "main.py")
	subDir := filepath.Join(tempWorkspace, "data")
	file2 := filepath.Join(subDir, "dataset.csv")

	assert.NoError(t, os.MkdirAll(subDir, 0755))
	assert.NoError(t, os.WriteFile(file1, []byte("print('hello')"), 0644))
	assert.NoError(t, os.WriteFile(file2, []byte("1,2,3"), 0644))

	// Assert they are present
	assert.FileExists(t, file1)
	assert.FileExists(t, file2)

	// 2. Execute Clean
	_, err := server.Clean(ctx, &pb.CleanRequest{})
	assert.NoError(t, err)

	// 3. Assert workspace is completely empty but still exists
	assert.DirExists(t, tempWorkspace)
	
	entries, err := os.ReadDir(tempWorkspace)
	assert.NoError(t, err)
	assert.Empty(t, entries)
}
