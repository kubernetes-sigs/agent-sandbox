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

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// Registry keeps track of available tools and handles their invocations.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry initializes a new tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Add registers a new tool.
func (r *Registry) Add(tool Tool) {
	if tool == nil {
		panic("registered tool must not be nil")
	}
	if reflect.TypeOf(tool).Kind() != reflect.Ptr {
		panic(fmt.Sprintf("registered tool %T must be a pointer", tool))
	}
	schema := tool.Schema()
	name := schema.Function.Name
	r.tools[name] = tool
}

// All returns schemas for all registered tools.
func (r *Registry) All() []llm.Tool {
	var list []llm.Tool
	for _, tool := range r.tools {
		list = append(list, tool.Schema())
	}
	slices.SortFunc(list, func(a, b llm.Tool) int {
		if a.Function.Name < b.Function.Name {
			return -1
		}
		if a.Function.Name > b.Function.Name {
			return 1
		}
		return 0
	})
	return list
}

// Call executes a tool call inside the provided sandbox and returns the LLM tool response message.
func (r *Registry) Call(ctx context.Context, sandbox Sandbox, tc llm.ToolCall) (llm.Message, error) {
	log := klog.FromContext(ctx)

	log.Info("llm invoking tool", "tool.name", tc.Function.Name)

	tool := r.tools[tc.Function.Name]
	if tool == nil {
		return llm.Message{}, fmt.Errorf("tool %q not found", tc.Function.Name)
	}

	toolType := reflect.TypeOf(tool).Elem()
	toolInvocation := reflect.New(toolType).Interface().(Tool)

	if err := json.Unmarshal([]byte(tc.Function.Arguments), toolInvocation); err != nil {
		return llm.Message{}, fmt.Errorf("failed to parse arguments: %w", err)
	}

	res, err := toolInvocation.Run(ctx, sandbox)
	if err != nil {
		return llm.Message{}, fmt.Errorf("failed to run tool: %w", err)
	}

	log.Info("tool result", "tool.name", tc.Function.Name)

	// Populate some default values so the tools don't all have to do it.
	if res.Role == "" {
		res.Role = "tool"
	}
	res.ToolCallID = tc.ID
	return res, nil
}
