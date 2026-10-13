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

// Package toolsets makes the tools and prompts of the sandboxed-tools example
// pluggable: a Toolset bundles the system prompt, the LLM-callable tools, and
// the sandbox image they expect, so different agent "personalities" (e.g. the
// basic example tools, or the gemini-cli tool surface) can be selected at
// startup without changing the agent loop.
package toolsets

import (
	"fmt"
	"strings"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/toolsets/basic"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/toolsets/geminicli"
)

// Toolset bundles the tools and prompts that define one agent profile.
type Toolset interface {
	// Name is the identifier used to select the toolset (e.g. on the CLI).
	Name() string

	// SystemPrompt returns the system prompt that seeds new sessions.
	SystemPrompt() string

	// RegisterTools adds the toolset's tools to the given registry.
	RegisterTools(registry *tools.Registry)

	// DefaultImage returns the sandbox container image this toolset expects,
	// or "" if any generic image will do.
	DefaultImage() string
}

// Get returns the toolset with the given name; an empty name selects the
// basic toolset.
func Get(name string) (Toolset, error) {
	switch name {
	case "", "basic":
		return basic.New(), nil
	case "geminicli":
		return geminicli.New(), nil
	default:
		return nil, fmt.Errorf("unknown toolset %q (available: %s)", name, strings.Join(Names(), ", "))
	}
}

// Names returns the names of all available toolsets.
func Names() []string {
	return []string{"basic", "geminicli"}
}
