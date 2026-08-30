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

package agtsbx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// defaultReadyTimeout bounds waiting for a sandbox to come up: long enough to
// cover an image pull on a cold host, short enough not to hang the terminal.
const defaultReadyTimeout = 3 * time.Minute

// generatedNamePrefix makes a leaked container or Sandbox object identifiable
// as this tool's doing.
const generatedNamePrefix = "agtsbx-"

// IO groups the streams the CLI reads and writes, so tests can capture them.
type IO struct {
	Stdout io.Writer
	Stderr io.Writer
}

// runOptions is the parsed form of `agtsbx run`'s command line.
type runOptions struct {
	Image     string
	Command   []string
	Runtime   string
	Namespace string
	Name      string
	Env       []string
	Workdir   string
	Keep      bool
	Quiet     bool
	GRPCPort  int
	RESTPort  int
	Timeout   time.Duration
}

// envFlag collects repeated -e/--env occurrences.
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

func (e *envFlag) Set(value string) error {
	key, _, found := strings.Cut(value, "=")
	if key == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", value)
	}
	if !found {
		// "-e KEY" forwards the host's value, as docker run does, keeping an
		// API key out of the command line and the shell history.
		hostValue, set := os.LookupEnv(key)
		if !set {
			return fmt.Errorf("%s is not set in the environment; pass %s=VALUE to set it explicitly", key, key)
		}
		value = key + "=" + hostValue
	}
	*e = append(*e, value)
	return nil
}

// ErrUsage signals a bad command line, so the caller can print usage rather
// than treat it as a runtime failure.
var ErrUsage = errors.New("usage")

// errHelpRequested is distinct from ErrUsage because asking for help is not a
// mistake: the text belongs on stdout and the exit status is success.
var errHelpRequested = errors.New("help requested")

const runUsage = `Usage: agtsbx run [OPTIONS] IMAGE COMMAND [ARG...]

Run a command in a throwaway sandbox and stream its output.

IMAGE must start sandboxd (the KEP-539.2 runtime daemon) as its entrypoint;
agtsbx talks to that daemon to execute COMMAND.

Options:
  --runtime string     Runtime to run the sandbox on: auto, docker, podman,
                       kubernetes (default "auto")
  -e, --env KEY=VALUE  Set an environment variable, or "-e KEY" to forward it
                       from the current environment (repeatable)
  -w, --workdir string Working directory for COMMAND, resolved inside the
                       sandbox root and required to exist there
  --name string        Name for the sandbox (default: generated)
  -n, --namespace string
                       Namespace for the kubernetes runtime (default "default")
  --keep               Leave the sandbox running after COMMAND exits
  --timeout duration   How long to wait for the sandbox to become ready
                       (default 3m0s)
  --grpc-port int      sandboxd ProcessService port inside the sandbox (default 9090)
  --rest-port int      sandboxd REST API port inside the sandbox (default 8080)
  -q, --quiet          Suppress progress messages on stderr

Examples:
  agtsbx run sandboxd:latest echo hello
  agtsbx run -e GREETING=hi sandboxd:latest sh -c 'echo $GREETING'
  agtsbx run --runtime kubernetes -n agents sandboxd:latest python3 -c 'print(1)'
`

// parseRunArgs parses the argument list for `agtsbx run`.
//
// FlagSet.Parse stops at the first non-flag argument, which is the image, and
// leaves the rest untouched. That is what lets `agtsbx run img ls -l` pass -l
// through to the sandboxed command instead of rejecting it.
func parseRunArgs(args []string, stderr io.Writer) (runOptions, error) {
	opts := runOptions{}
	var env envFlag

	set := flag.NewFlagSet("run", flag.ContinueOnError)
	set.SetOutput(stderr)
	// The caller prints the usage text; suppress the flag package's own
	// listing so it is not printed twice.
	set.Usage = func() {}

	set.StringVar(&opts.Runtime, "runtime", RuntimeAuto, "runtime to run the sandbox on")
	set.Var(&env, "e", "environment variable KEY=VALUE")
	set.Var(&env, "env", "environment variable KEY=VALUE")
	set.StringVar(&opts.Workdir, "w", "", "working directory inside the sandbox root")
	set.StringVar(&opts.Workdir, "workdir", "", "working directory inside the sandbox root")
	set.StringVar(&opts.Name, "name", "", "name for the sandbox")
	set.StringVar(&opts.Namespace, "n", "default", "namespace for the kubernetes runtime")
	set.StringVar(&opts.Namespace, "namespace", "default", "namespace for the kubernetes runtime")
	set.BoolVar(&opts.Keep, "keep", false, "leave the sandbox running after the command exits")
	set.BoolVar(&opts.Quiet, "q", false, "suppress progress messages")
	set.BoolVar(&opts.Quiet, "quiet", false, "suppress progress messages")
	set.IntVar(&opts.GRPCPort, "grpc-port", defaultGRPCPort, "sandboxd gRPC port inside the sandbox")
	set.IntVar(&opts.RESTPort, "rest-port", defaultRESTPort, "sandboxd REST port inside the sandbox")
	set.DurationVar(&opts.Timeout, "timeout", defaultReadyTimeout, "how long to wait for the sandbox to be ready")

	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return runOptions{}, errHelpRequested
		}
		return runOptions{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}

	rest := set.Args()
	if len(rest) == 0 {
		return runOptions{}, fmt.Errorf("%w: an IMAGE is required", ErrUsage)
	}
	opts.Image = rest[0]
	opts.Command = rest[1:]

	if len(opts.Command) == 0 {
		// The entrypoint is sandboxd, so no command would just start a
		// daemon and appear to hang.
		return runOptions{}, fmt.Errorf("%w: a COMMAND is required", ErrUsage)
	}
	if !isKnownRuntime(opts.Runtime) {
		return runOptions{}, fmt.Errorf("%w: unknown runtime %q, expected one of %v", ErrUsage, opts.Runtime, knownRuntimes)
	}
	if opts.Timeout <= 0 {
		return runOptions{}, fmt.Errorf("%w: --timeout must be positive, got %s", ErrUsage, opts.Timeout)
	}
	if err := validatePort("--grpc-port", opts.GRPCPort); err != nil {
		return runOptions{}, err
	}
	if err := validatePort("--rest-port", opts.RESTPort); err != nil {
		return runOptions{}, err
	}
	if opts.GRPCPort == opts.RESTPort {
		return runOptions{}, fmt.Errorf("%w: --grpc-port and --rest-port must differ (both are %d)", ErrUsage, opts.GRPCPort)
	}

	opts.Env = env
	if opts.Name == "" {
		name, err := generateName()
		if err != nil {
			return runOptions{}, err
		}
		opts.Name = name
	}
	return opts, nil
}

// validatePort rejects out-of-range ports here, where the message is clearer
// than the engine's or the API server's.
func validatePort(flagName string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: %s must be between 1 and 65535, got %d", ErrUsage, flagName, port)
	}
	return nil
}

// generateName produces a unique, DNS-label-safe sandbox name: it is used as
// both a container name and a Kubernetes object name.
func generateName() (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generating sandbox name: %w", err)
	}
	return generatedNamePrefix + hex.EncodeToString(suffix), nil
}

// Run executes `agtsbx run` and returns the exit code the process should use.
// On success that is the sandboxed command's own, so agtsbx composes in shell
// pipelines the way docker run does.
func Run(ctx context.Context, args []string, streams IO) (int, error) {
	opts, err := parseRunArgs(args, streams.Stderr)
	if errors.Is(err, errHelpRequested) {
		fmt.Fprint(streams.Stdout, runUsage)
		return 0, nil
	}
	if err != nil {
		if errors.Is(err, ErrUsage) {
			fmt.Fprintf(streams.Stderr, "agtsbx: %v\n\n%s", err, runUsage)
		}
		return 1, err
	}

	progress := streams.Stderr
	if opts.Quiet {
		progress = io.Discard
	}

	runtimeName, err := selectRuntime(opts.Runtime, defaultLookPath)
	if err != nil {
		return 1, err
	}

	backend, err := newBackend(runtimeName, backendOptions{
		Namespace:    opts.Namespace,
		ReadyTimeout: opts.Timeout,
		Stderr:       progress,
	})
	if err != nil {
		return 1, err
	}

	// opts.Env is not part of the spec: it reaches the sandbox with the
	// command below, so no credential lands in the engine argv or the
	// Sandbox object.
	spec := Spec{
		Image:    opts.Image,
		Name:     opts.Name,
		GRPCPort: opts.GRPCPort,
		RESTPort: opts.RESTPort,
		Remove:   !opts.Keep,
	}

	fmt.Fprintf(progress, "agtsbx: starting %s on %s as %s\n", spec.Image, backend.Name(), spec.Name)
	instance, err := backend.Start(ctx, spec)
	if err != nil {
		return 1, err
	}
	defer func() {
		// Teardown must not inherit ctx: a cancelled ctx usually means
		// Ctrl-C, when the sandbox most needs removing.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := instance.Stop(stopCtx); err != nil {
			fmt.Fprintf(streams.Stderr, "agtsbx: warning: %v\n", err)
		}
	}()

	if opts.Keep {
		fmt.Fprintf(progress, "agtsbx: sandbox %s left running (--keep)\n", spec.Name)
	}

	exitCode, err := runCommand(ctx, instance.Endpoints().GRPCAddr, execRequest{
		Command: opts.Command,
		Env:     opts.Env,
		Workdir: opts.Workdir,
		Stdout:  streams.Stdout,
		Stderr:  streams.Stderr,
	})
	if err != nil {
		return 1, err
	}
	return exitCode, nil
}
