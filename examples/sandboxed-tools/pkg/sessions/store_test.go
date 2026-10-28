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

package sessions

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

func TestFileStore(t *testing.T) {
	ctx := t.Context()
	tempDir, err := os.MkdirTemp("", "sessions-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := NewFileStore(tempDir)

	sessionID := "testsession123"

	// 1. Load non-existent session
	msgs, meta, err := store.LoadSession(ctx, sessionID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadSession for non-existent session: got err %v, want ErrNotFound", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected empty messages, got %d", len(msgs))
	}
	if meta != nil {
		t.Errorf("expected nil metadata for non-existent session, got %+v", meta)
	}

	// 2. Append a message
	sysPrompt := "You are a helpful assistant."
	msg1 := llm.Message{
		Role:    "system",
		Content: &sysPrompt,
	}
	if err := store.AppendMessages(ctx, sessionID, msg1); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	// 3. Load and assert msg1
	msgs, meta, err = store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	if diff := cmp.Diff([]llm.Message{msg1}, msgs); diff != "" {
		t.Errorf("messages mismatch (-want +got):\n%s", diff)
	}
	if meta == nil || *meta != (Metadata{}) {
		t.Errorf("expected empty metadata when only messages exist, got %+v", meta)
	}

	// 4. Append more messages (user and assistant with tool calls)
	userPrompt := "list files"
	msg2 := llm.Message{
		Role:    "user",
		Content: &userPrompt,
	}
	if err := store.AppendMessages(ctx, sessionID, msg2); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	toolCall := llm.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "list_files",
			Arguments: `{"path": "."}`,
		},
	}
	msg3 := llm.Message{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{toolCall},
	}

	toolResult := "file1.txt\nfile2.txt"
	msg4 := llm.Message{
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    &toolResult,
	}
	if err := store.AppendMessages(ctx, sessionID, msg3, msg4); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	// 5. Load and assert everything
	msgs, meta, err = store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	wantMsgs := []llm.Message{msg1, msg2, msg3, msg4}
	if diff := cmp.Diff(wantMsgs, msgs); diff != "" {
		t.Errorf("messages mismatch (-want +got):\n%s", diff)
	}
	if meta == nil || *meta != (Metadata{}) {
		t.Errorf("expected empty metadata when only messages exist, got %+v", meta)
	}
}

func TestFileStoreMetadata(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(t.TempDir())

	// A legacy session (messages recorded before metadata existed) returns an
	// empty Metadata from LoadSession.
	content := "hi"
	if err := store.AppendMessages(ctx, "legacy", llm.Message{Role: "user", Content: &content}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, legacyMeta, err := store.LoadSession(ctx, "legacy")
	if err != nil {
		t.Fatalf("LoadSession(legacy): %v", err)
	}
	if legacyMeta == nil || *legacyMeta != (Metadata{}) {
		t.Errorf("LoadSession(legacy) metadata = %+v, want empty Metadata", legacyMeta)
	}

	want := Metadata{
		Parent:    "parent1",
		Toolset:   "geminicli",
		CreatedAt: time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC),
	}

	// Reusing a session ID that already has messages must fail.
	if err := store.NewSession(ctx, "legacy", want); err == nil {
		t.Error("NewSession on existing legacy session should fail")
	}

	if err := store.NewSession(ctx, "child1", want); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Loading a newly created session returns empty messages and the stored
	// metadata.
	msgs, gotMeta, err := store.LoadSession(ctx, "child1")
	if err != nil {
		t.Fatalf("LoadSession after NewSession: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("LoadSession after NewSession messages = %v, want empty", msgs)
	}
	if diff := cmp.Diff(want, *gotMeta); diff != "" {
		t.Errorf("LoadSession metadata mismatch (-want +got):\n%s", diff)
	}

	// Reusing a session ID that already has metadata must fail.
	if err := store.NewSession(ctx, "child1", want); err == nil {
		t.Error("NewSession with a reused session ID should fail")
	}

	if err := store.NewSession(ctx, "Not Valid", want); err == nil {
		t.Error("NewSession with an invalid session name should fail")
	}
}
