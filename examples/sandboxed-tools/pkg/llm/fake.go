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
	"fmt"
	"os"
	"slices"
	"strings"
)

// ChatClient is implemented by chat completion backends: the real
// OpenAI-compatible Client, or a fake for testing.
type ChatClient interface {
	CreateChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error)
}

var _ ChatClient = &Client{}
var _ ChatClient = &ElizaClient{}

// FakeModelEliza is the model name that selects the built-in fake LLM
// instead of a real API. Use it (e.g. OPENAI_MODEL=fake-eliza) in tests and
// demos that should not depend on LLM availability or an API key.
const FakeModelEliza = "fake-eliza"

// NewFromEnv returns the ChatClient for modelName: the built-in fake for
// FakeModelEliza (which needs no API key), otherwise the real
// OpenAI-compatible client configured from the GEMINI_API_KEY /
// OPENAI_API_KEY and OPENAI_BASE_URL environment variables.
func NewFromEnv(modelName string) (ChatClient, error) {
	if modelName == FakeModelEliza {
		return &ElizaClient{}, nil
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY or OPENAI_API_KEY environment variable is required")
	}

	return NewClient(os.Getenv("OPENAI_BASE_URL"), apiKey)
}

// ElizaClient is a fake ChatClient for testing the agent plumbing without a
// real LLM. Like its namesake, it answers every message with a question
// reflecting the user's words back:
//
//	"What is the capital of France?" =>
//	"What do you think it means when you say 'What is the capital of France?'?"
//
// It is deterministic. It knows one trick beyond conversation, so that the
// tool execution path can be tested end to end: a message of the form
// "run: <command>" makes it request a run_command tool call, and it reports
// the tool's result on the next iteration.
type ElizaClient struct{}

// runPrefix triggers ElizaClient's run_command tool call.
const runPrefix = "run:"

// CreateChatCompletion implements ChatClient.
func (c *ElizaClient) CreateChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	reply := "How does that make you feel?"

	if len(req.Messages) > 0 {
		if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" {
			// We just ran a tool; report its result.
			reply = fmt.Sprintf("The tool told me:\n%s", valueOf(last.Content))
			return textResponse(reply), nil
		}
	}

	lastUser := ""
	for _, msg := range slices.Backward(req.Messages) {
		if msg.Role == "user" && msg.Content != nil {
			lastUser = *msg.Content
			break
		}
	}

	if command, ok := strings.CutPrefix(lastUser, runPrefix); ok {
		return runCommandResponse(strings.TrimSpace(command))
	}

	if lastUser != "" {
		reply = fmt.Sprintf("What do you think it means when you say '%s'?", lastUser)
	}
	return textResponse(reply), nil
}

func textResponse(reply string) *ChatCompletionResponse {
	return &ChatCompletionResponse{
		ID: "fake-eliza",
		Choices: []Choice{
			{
				Message:      Message{Role: "assistant", Content: &reply},
				FinishReason: "stop",
			},
		},
	}
}

// runCommandResponse asks the harness to execute command via the
// run_command tool, exactly as a real LLM would.
func runCommandResponse(command string) (*ChatCompletionResponse, error) {
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return nil, fmt.Errorf("marshaling run_command arguments: %w", err)
	}
	return &ChatCompletionResponse{
		ID: "fake-eliza",
		Choices: []Choice{
			{
				Message: Message{
					Role: "assistant",
					ToolCalls: []ToolCall{
						{
							ID:   "eliza-run-1",
							Type: "function",
							Function: FunctionCall{
								Name:      "run_command",
								Arguments: string(args),
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}, nil
}

// valueOf safely dereferences p, returning the zero value if p is nil.
func valueOf[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
