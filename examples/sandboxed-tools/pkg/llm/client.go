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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Client interacts with an OpenAI-compatible API endpoint using net/http.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// ChatCompletionRequest represents the request body for the chat completions API.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

// Message represents a single chat message in the conversation history.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ThoughtSignature string     `json:"thought_signature,omitempty"`
	ExtraContent     any        `json:"extra_content,omitempty"`
}

// Tool defines a tool available for the LLM to call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes the function specification for a tool.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ToolCall represents a function call requested by the LLM.
type ToolCall struct {
	ID               string       `json:"id"`
	Type             string       `json:"type"`
	Function         FunctionCall `json:"function"`
	ThoughtSignature string       `json:"thought_signature,omitempty"`
	ExtraContent     any          `json:"extra_content,omitempty"`
}

// FunctionCall contains the name and arguments of the requested function.
type FunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// ChatCompletionResponse represents the response body from the chat completions API.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
}

// Choice represents a single completion choice returned by the model.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// NewClient initializes a new OpenAI-compatible LLM client.
func NewClient(baseURL, apiKey string) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("llm: api key is required")
	}
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	// Set reasonable timeouts just to avoid hanging on requests for too long.
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
	}

	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: httpClient,
	}, nil
}

// CreateChatCompletion sends a chat completion request to the API.
func (c *Client) CreateChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	log := klog.FromContext(ctx)

	url := c.baseURL + "/chat/completions"
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: failed to marshal chat completion request: %w", err)
	}
	log.V(1).Info("making LLM request", "request", string(bodyBytes))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("llm: failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: failed to read response body: %w", err)
	}
	log.V(1).Info("response from LLM", "status", resp.StatusCode, "body", string(respBody))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: api error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("llm: failed to decode response: %w", err)
	}

	return &chatResp, nil
}
