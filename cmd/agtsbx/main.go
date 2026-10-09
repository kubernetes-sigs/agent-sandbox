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

// Binary agtsbx is a docker-like command-line front end for agent-sandbox.
//
//	agtsbx run [OPTIONS] IMAGE COMMAND [ARG...]
//
// It starts a sandbox from a container image, waits for the sandboxd runtime
// daemon inside it to report healthy, runs one command through sandboxd's
// ProcessService and streams the output back. The sandbox is torn down when
// the command exits.
//
// The sandbox runs as a local Docker or Podman container, or as a Sandbox
// object in a Kubernetes cluster. Every backend exposes the same KEP-539.2
// runtime API, so a command behaves identically wherever it ran.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/agent-sandbox/internal/agtsbx"
	"sigs.k8s.io/agent-sandbox/internal/version"
)

const usage = `agtsbx runs commands in agent sandboxes.

Usage:
  agtsbx run [OPTIONS] IMAGE COMMAND [ARG...]   Run a command in a sandbox
  agtsbx version                                Print version information
  agtsbx help                                   Show this message

Run "agtsbx run --help" for the options of the run command.
`

func main() {
	os.Exit(run())
}

// run holds the body of main so deferred cleanup still executes; os.Exit
// would skip it.
func run() int {
	// Ctrl-C cancels the context rather than killing the process outright, so
	// the sandbox is torn down instead of leaking.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Deregister as soon as the first signal lands, so a second one takes the
	// default action instead of being swallowed by a stalled teardown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	switch args[0] {
	case "run":
		streams := agtsbx.IO{Stdout: os.Stdout, Stderr: os.Stderr}
		exitCode, err := agtsbx.Run(ctx, args[1:], streams)
		if err != nil {
			// Usage errors have already printed their own guidance.
			if !errors.Is(err, agtsbx.ErrUsage) {
				fmt.Fprintf(os.Stderr, "agtsbx: %v\n", err)
			}
			return exitCode
		}
		return exitCode
	case "version", "--version", "-v":
		fmt.Fprintln(os.Stdout, version.Print("agtsbx"))
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "agtsbx: unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}
