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

package geminicli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

// ShellToolName matches gemini-cli's shell tool name.
const ShellToolName = "run_shell_command"

// maxShellOutputSize limits the maximum size of the output of shell commands.
const maxShellOutputSize = 8 * (1 << 20) // 8 MB

// RunShellCommandTool executes a bash command inside the sandbox, mirroring
// gemini-cli's run_shell_command tool. Commands run in the sandbox home
// directory (the persisted workspace) unless dir_path says otherwise.
//
// TODO: gemini-cli's shell tool also reports "Background PIDs" for any
// processes left running and the process-group PGID (so the model can
// `kill -- -PGID`), and truncates very large command output. We only report
// the background PID and log file from the is_background path.
//
// TODO: gemini-cli's schema also has a `delay_ms` parameter (how long to wait
// for initial output after starting a background process; ours is fixed at
// 1s in backgroundScript) and, in parallel-tool-call configurations, a
// `wait_for_previous` parameter (our harness executes tool calls
// sequentially, so it is not needed yet).
type RunShellCommandTool struct {
	// Command is the exact bash command to execute.
	Command string `json:"command"`
	// Description is a brief user-facing description of the command.
	Description string `json:"description,omitempty"`
	// DirPath is the directory to run the command in, relative to the
	// workspace root (the sandbox home directory).
	DirPath string `json:"dir_path,omitempty"`
	// IsBackground runs the command as a detached background process.
	IsBackground bool `json:"is_background,omitempty"`
}

func (t *RunShellCommandTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: ShellToolName,
			Description: "This tool executes a given shell command as `bash -c <command>` inside the sandbox. To run a command in the background, set the `is_background` parameter to true; do NOT use `&` to background commands yourself.\n\n" +
				"The following information is returned:\n\n" +
				"Output: Combined stdout/stderr. Can be `(empty)`.\n" +
				"Exit Code: Only included if non-zero (command failed).",
			Parameters: objectSchema(map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Exact bash command to execute as `bash -c <command>`",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Brief description of the command for the user. Be specific and concise. Ideally a single sentence. Can be up to 3 sentences for clarity. No line breaks.",
				},
				"dir_path": map[string]any{
					"type":        "string",
					"description": "(OPTIONAL) The path of the directory to run the command in. If not provided, the workspace root directory is used. Must already exist.",
				},
				"is_background": map[string]any{
					"type":        "boolean",
					"description": "Set to true if this command should be run in the background (e.g. for long-running servers or watchers). The command will be started, allowed to run for a brief moment to check for immediate errors, and then left running in the background.",
				},
			}, "command"),
		},
	}
}

// foregroundScript runs the model's command ($2) in the requested directory
// ($1, defaulting to the sandbox home). It is executed as
// `bash -c <script> <name> <dir> <command>`.
const foregroundScript = `cd -- "${HOME:-/}" || exit 1
if [ -n "$1" ]; then cd -- "$1" || exit 1; fi
exec bash -c "$2"`

// backgroundScript starts the model's command detached from the exec session
// (so it survives the kubernetes exec connection closing), waits briefly, and
// reports the PID plus any initial output.
const backgroundScript = `cd -- "${HOME:-/}" || exit 1
if [ -n "$1" ]; then cd -- "$1" || exit 1; fi
logfile="$(mktemp /tmp/shell-bg-XXXXXX.log)"
setsid bash -c "$2" </dev/null >"$logfile" 2>&1 &
pid=$!
sleep 1
echo "Started background process with PID ${pid}. Output is being logged to ${logfile}."
echo "--- initial output ---"
tail -c 2000 -- "$logfile"`

func (t *RunShellCommandTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	log := klog.FromContext(ctx)

	if t.Command == "" {
		return llm.Message{}, errors.New("command is required")
	}

	// Don't log command because it may contain secrets.
	log.Info("executing shell command in sandbox", "background", t.IsBackground)

	script := foregroundScript
	if t.IsBackground {
		script = backgroundScript
	}

	// The shell tool reports combined stdout/stderr, so stream both into one
	// buffer; syncWriter serializes the two streams.
	combined := tools.NewLimitedWriter(maxShellOutputSize)
	res, err := sandbox.ExecCommand(ctx, tools.ExecCommandOptions{
		Command: []string{"bash", "-c", script, ShellToolName, t.DirPath, t.Command},
		Stdout:  combined,
		Stderr:  combined,
	})
	if err != nil {
		return llm.Message{}, err
	}

	output := combined.String()
	if strings.TrimSpace(output) == "" {
		output = "(empty)"
	}

	if combined.Truncated() {
		output += fmt.Sprintf("\n[Output truncated at %d bytes]", combined.Len())
	}
	content := "Output: " + output
	if res.ExitCode != 0 {
		content += fmt.Sprintf("\nExit Code: %d", res.ExitCode)
	}
	return llm.Message{Content: &content}, nil
}
