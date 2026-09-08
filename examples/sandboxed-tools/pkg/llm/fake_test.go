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

package llm

import (
	"context"
	"encoding/json"
	"testing"
)

func userMessage(text string) Message {
	return Message{Role: "user", Content: &text}
}

func TestElizaReflectsUserMessage(t *testing.T) {
	client := &ElizaClient{}

	resp, err := client.CreateChatCompletion(context.Background(), ChatCompletionRequest{
		Messages: []Message{userMessage("What is the capital of France?")},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}

	got := *resp.Choices[0].Message.Content
	want := "What do you think it means when you say 'What is the capital of France?'?"
	if got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("unexpected tool calls: %v", resp.Choices[0].Message.ToolCalls)
	}
}

func TestElizaRunCommand(t *testing.T) {
	client := &ElizaClient{}

	resp, err := client.CreateChatCompletion(context.Background(), ChatCompletionRequest{
		Messages: []Message{userMessage("run: uname -a")},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}

	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls = %v, want exactly one", toolCalls)
	}
	if toolCalls[0].Function.Name != "run_command" {
		t.Errorf("tool name = %q, want run_command", toolCalls[0].Function.Name)
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("unmarshaling arguments %q: %v", toolCalls[0].Function.Arguments, err)
	}
	if args.Command != "uname -a" {
		t.Errorf("command = %q, want %q", args.Command, "uname -a")
	}
}

func TestElizaReportsToolResult(t *testing.T) {
	client := &ElizaClient{}

	toolOutput := "stdout:\nLinux"
	resp, err := client.CreateChatCompletion(context.Background(), ChatCompletionRequest{
		Messages: []Message{
			userMessage("run: uname"),
			{Role: "tool", ToolCallID: "eliza-run-1", Content: &toolOutput},
		},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}

	got := *resp.Choices[0].Message.Content
	want := "The tool told me:\nstdout:\nLinux"
	if got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
}
