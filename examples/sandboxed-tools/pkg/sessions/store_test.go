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
	"context"
	"os"
	"testing"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	tempDir, err := os.MkdirTemp("", "sessions-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := NewFileStore(tempDir)

	sessionID := "test-session-123"

	// 1. Load non-existent session
	msgs, err := store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession failed for non-existent session: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected empty messages, got %d", len(msgs))
	}

	// 2. Append a message
	sysPrompt := "You are a helpful assistant."
	msg1 := llm.Message{
		Role:    "system",
		Content: &sysPrompt,
	}
	if err := store.AppendMessage(ctx, sessionID, msg1); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	// 3. Load and assert msg1
	msgs, err = store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content == nil || *msgs[0].Content != sysPrompt {
		t.Errorf("loaded message does not match: %+v", msgs[0])
	}

	// 4. Append more messages (user and assistant with tool calls)
	userPrompt := "list files"
	msg2 := llm.Message{
		Role:    "user",
		Content: &userPrompt,
	}
	if err := store.AppendMessage(ctx, sessionID, msg2); err != nil {
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
	if err := store.AppendMessage(ctx, sessionID, msg3); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	toolResult := "file1.txt\nfile2.txt"
	msg4 := llm.Message{
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    &toolResult,
	}
	if err := store.AppendMessage(ctx, sessionID, msg4); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	// 5. Load and assert everything
	msgs, err = store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	if msgs[1].Role != "user" || msgs[1].Content == nil || *msgs[1].Content != userPrompt {
		t.Errorf("message 2 mismatch: %+v", msgs[1])
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].ID != "call_1" {
		t.Errorf("message 3 mismatch: %+v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || msgs[3].Content == nil || *msgs[3].Content != toolResult {
		t.Errorf("message 4 mismatch: %+v", msgs[3])
	}
}
