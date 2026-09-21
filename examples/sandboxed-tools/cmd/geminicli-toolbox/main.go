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

// geminicli-toolbox implements the filesystem tools of the geminicli toolset.
// It runs inside the sandbox: the host-side agent execs
// `geminicli-toolbox <tool-name>` with the LLM's tool arguments as JSON on
// stdin, and reads the model-facing result from stdout. A non-zero exit code
// indicates the tool call failed; the reason is written to stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/toolsets/geminicli/toolbox"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Relative paths in tool arguments resolve against the workspace root:
	// by default the sandbox home directory, which is the directory the
	// sandboxed-tools agent persists across sandbox restarts.
	workdir := os.Getenv("HOME")
	flag.StringVar(&workdir, "workdir", workdir, "workspace root that relative paths resolve against")
	flag.Parse()

	if flag.NArg() != 1 {
		return fmt.Errorf("usage: geminicli-toolbox [--workdir=DIR] TOOL_NAME (tool parameters as JSON on stdin)")
	}
	toolName := flag.Arg(0)

	if workdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot determine workspace root ($HOME and cwd unavailable): %w", err)
		}
		workdir = cwd
	}

	params, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read tool parameters from stdin: %w", err)
	}

	result, err := toolbox.Run(ctx, workdir, toolName, params)
	if err != nil {
		return err
	}

	if _, err := os.Stdout.WriteString(result); err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}
	if !strings.HasSuffix(result, "\n") {
		fmt.Println()
	}
	return nil
}
