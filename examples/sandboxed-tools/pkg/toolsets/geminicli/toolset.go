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

// Package geminicli is a toolset that reproduces the gemini-cli tool surface
// (https://github.com/google-gemini/gemini-cli, Apache-2.0) for the
// sandboxed-tools example: the same tool names, schemas, and system prompt,
// with the tools reimplemented in Go so no Node.js runtime is needed in the
// sandbox.
//
// The filesystem tools (list_directory, read_file, write_file, glob,
// grep_search, replace, read_many_files) are implemented by the
// geminicli-toolbox binary running inside the sandbox (see the toolbox
// subpackage and examples/sandboxed-tools/cmd/geminicli-toolbox);
// run_shell_command executes bash directly. The sandbox image must therefore
// provide bash and the geminicli-toolbox binary — see
// examples/sandboxed-tools/images/geminicli-toolbox.
package geminicli

import (
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

// DefaultImage is the sandbox image expected by this toolset. It is not
// published to a registry: build it from the repo root and load it into your
// cluster (see examples/sandboxed-tools/images/geminicli-toolbox/Dockerfile).
const DefaultImage = "geminicli-toolbox:latest"

// Toolset exposes the gemini-cli tools and prompts.
type Toolset struct{}

// New returns the geminicli toolset.
func New() *Toolset {
	return &Toolset{}
}

// Name implements toolsets.Toolset.
func (t *Toolset) Name() string {
	return "geminicli"
}

// SystemPrompt implements toolsets.Toolset.
func (t *Toolset) SystemPrompt() string {
	return systemPrompt
}

// RegisterTools implements toolsets.Toolset.
//
// This registers the subset of gemini-cli's tools that make sense in a
// headless sandbox and that we have implemented so far. The remaining gaps,
// from gemini-cli's full tool manifest
// (packages/core/src/tools/definitions/base-declarations.ts), are listed
// below; we may never implement some of them, but the list is the inventory
// of what is missing:
//
// TODO: web_fetch - fetch URL(s) and process their content with a prompt.
// Needs outbound network access from the sandbox (or host-side fetching) and
// a summarization step; gemini-cli delegates the processing to the model.
//
// TODO: google_web_search - web search via the Gemini API's Google Search
// grounding. Requires Gemini API-specific support; there is no OpenAI-compat
// equivalent, so this would need a separate Gemini API client or a different
// search backend.
//
// TODO: write_todos - subtask/progress tracking. Purely host-side state (no
// sandbox involvement); needs somewhere to store the todo list on the
// Session and a way to render it in the CLI.
//
// TODO: ask_user - structured questions back to the user (choice/text/yesno).
// Needs harness support: a TurnEvents extension so front ends can prompt the
// user and return the answer as the tool result.
//
// TODO: run_shell_command is_background parity - gemini-cli reports
// background PIDs and the process-group PGID and offers wait/kill management
// of background processes; we only report the PID and a log file (see
// shell.go).
//
// TODO: enter_plan_mode / exit_plan_mode - read-only planning mode with a
// plan file and an approval gate. Needs harness support (tool filtering per
// mode and an approval flow), not just a tool implementation.
//
// TODO: activate_skill - agent skills loaded from the workspace
// (.gemini/skills, and .agents/skills in newer layouts). Needs skill
// discovery in the sandbox plus prompt wiring for <available_skills>.
//
// TODO: save_memory / GEMINI.md context files - gemini-cli persists
// long-lived context by loading GEMINI.md files into the prompt and letting
// the model edit them. Our sessions only persist chat history and the
// sandbox filesystem; loading GEMINI.md from the sandbox home directory into
// the system prompt would be the natural equivalent (see prompt.go).
//
// Not planned (gemini-cli-internal or out of scope for this example):
// get_internal_docs, update_topic, complete_task, the subagent/tracker tools
// (agent, tracker_create_task, tracker_list_tasks, tracker_update_task), and
// the MCP tools (read_mcp_resource, list_mcp_resources).
func (t *Toolset) RegisterTools(registry *tools.Registry) {
	registry.Add(&ListDirectoryTool{})
	registry.Add(&ReadFileTool{})
	registry.Add(&WriteFileTool{})
	registry.Add(&GlobTool{})
	registry.Add(&GrepTool{})
	registry.Add(&ReplaceTool{})
	registry.Add(&ReadManyFilesTool{})
	registry.Add(&RunShellCommandTool{})
}

// DefaultImage implements toolsets.Toolset.
func (t *Toolset) DefaultImage() string {
	return DefaultImage
}
