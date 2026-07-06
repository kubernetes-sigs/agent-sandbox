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
	"io"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	filesystemv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/filesystem/v1"
)

// mockWriteServer implements filesystemv1.FilesystemService_WriteServer for testing Write streaming.
type mockWriteServer struct {
	grpc.ServerStream
	ctx      context.Context
	requests []*filesystemv1.WriteRequest
	index    int
}

func (m *mockWriteServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockWriteServer) Recv() (*filesystemv1.WriteRequest, error) {
	if m.index >= len(m.requests) {
		return nil, io.EOF
	}
	req := m.requests[m.index]
	m.index++
	return req, nil
}

func (m *mockWriteServer) SendAndClose(res *filesystemv1.WriteResponse) error {
	return nil
}

// mockReadServer implements filesystemv1.FilesystemService_ReadServer for testing Read streaming.
type mockReadServer struct {
	grpc.ServerStream
	ctx        context.Context
	sentChunks [][]byte
}

func (m *mockReadServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockReadServer) Send(res *filesystemv1.ReadResponse) error {
	m.sentChunks = append(m.sentChunks, res.GetChunk())
	return nil
}

func TestFilesystemServer(t *testing.T) {
	tempDir := t.TempDir()
	fsServer := NewFilesystemServer(tempDir)
	ctx := context.Background()

	// 1. Test MakeDir
	subDirRel := "testdir/subdir"
	_, err := fsServer.MakeDir(ctx, &filesystemv1.MakeDirRequest{Path: subDirRel})
	if err != nil {
		t.Fatalf("MakeDir failed: %v", err)
	}

	fullSubDir := filepath.Join(tempDir, subDirRel)
	if info, err := os.Stat(fullSubDir); err != nil || !info.IsDir() {
		t.Fatalf("MakeDir did not create directory: %v", err)
	}

	// 2. Test Write
	filePathRel := "testdir/subdir/hello.txt"
	content := []byte("Hello, sandboxd filesystem!")
	writeStream := &mockWriteServer{
		requests: []*filesystemv1.WriteRequest{
			{Payload: &filesystemv1.WriteRequest_Metadata{Metadata: &filesystemv1.FileMetadata{Path: filePathRel}}},
			{Payload: &filesystemv1.WriteRequest_Chunk{Chunk: content}},
		},
	}

	if err := fsServer.Write(writeStream); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 3. Test Stat
	statRes, err := fsServer.Stat(ctx, &filesystemv1.StatRequest{Path: filePathRel})
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if statRes.GetType() != filesystemv1.FileType_FILE_TYPE_FILE {
		t.Errorf("expected file type FILE, got %v", statRes.GetType())
	}
	if statRes.GetSize() != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), statRes.GetSize())
	}

	// 4. Test Read
	readStream := &mockReadServer{}
	if err := fsServer.Read(&filesystemv1.ReadRequest{Path: filePathRel}, readStream); err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	var readData []byte
	for _, chunk := range readStream.sentChunks {
		readData = append(readData, chunk...)
	}
	if string(readData) != string(content) {
		t.Errorf("expected content %q, got %q", string(content), string(readData))
	}

	// 5. Test List
	listRes, err := fsServer.List(ctx, &filesystemv1.ListRequest{Path: "testdir/subdir"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listRes.GetEntries()) != 1 || listRes.GetEntries()[0].GetName() != "hello.txt" {
		t.Errorf("unexpected List entries: %v", listRes.GetEntries())
	}
	if listRes.GetTruncated() {
		t.Errorf("expected truncated to be false")
	}

	// 6. Test Remove
	_, err = fsServer.Remove(ctx, &filesystemv1.RemoveRequest{Path: "testdir", Recursive: true})
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "testdir")); !os.IsNotExist(err) {
		t.Errorf("Remove did not delete directory")
	}
}
