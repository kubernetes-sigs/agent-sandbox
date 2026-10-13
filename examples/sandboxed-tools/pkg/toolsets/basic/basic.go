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

// Package basic is the original sandboxed-tools toolset: a minimal set of
// generic tools (run_command, ls, read, write) with a short system prompt.
// It works with any sandbox image that provides standard POSIX utilities.
package basic

import (
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

// systemPrompt seeds every new session that uses the basic toolset.
const systemPrompt = "You are a helpful AI assistant with access to a sandboxed environment. " +
	"You can use the available tools (like run_command to execute shell commands, ls to list files, read to read files, and write to write files) to answer user questions or perform tasks. " +
	"Always explain what you are doing."

// Toolset is the basic sandboxed-tools toolset.
type Toolset struct{}

// New returns the basic toolset.
func New() *Toolset {
	return &Toolset{}
}

// Name implements toolsets.Toolset.
func (t *Toolset) Name() string {
	return "basic"
}

// SystemPrompt implements toolsets.Toolset.
func (t *Toolset) SystemPrompt() string {
	return systemPrompt
}

// RegisterTools implements toolsets.Toolset.
func (t *Toolset) RegisterTools(registry *tools.Registry) {
	registry.Add(&tools.RunCommand{})
	registry.Add(&tools.ListFilesTool{})
	registry.Add(&tools.ReadFileTool{})
	registry.Add(&tools.WriteFileTool{})
}

// DefaultImage implements toolsets.Toolset. The basic tools only need
// standard POSIX utilities, so any generic image will do.
func (t *Toolset) DefaultImage() string {
	return ""
}
