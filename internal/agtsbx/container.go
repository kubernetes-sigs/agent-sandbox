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
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// managedByLabel marks containers agtsbx created. See managedByValue.
const managedByLabel = "app.kubernetes.io/managed-by"

// containerBackend runs the sandbox as a local container.
//
// It drives the engine CLI rather than linking an engine SDK: the client
// libraries are large, and podman comes free because the subcommands used
// here are identical in both engines.
type containerBackend struct {
	// engine is the executable name, also the backend name ("docker"/"podman").
	engine string
	runner commandRunner
	// httpClient probes sandboxd readiness on the published port.
	httpClient   *http.Client
	readyTimeout time.Duration
	progress     writerFunc
}

// commandRunner executes an engine subcommand and returns its stdout, so the
// backend can be tested with no engine installed.
type commandRunner interface {
	run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production commandRunner.
type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The engine's diagnostics beat a bare exit status.
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
	}
	return stdout.Bytes(), nil
}

func newContainerBackend(engine string, opts backendOptions) *containerBackend {
	return &containerBackend{
		engine:       engine,
		runner:       execRunner{},
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		readyTimeout: opts.ReadyTimeout,
		progress:     opts.Stderr,
	}
}

func (b *containerBackend) Name() string { return b.engine }

// containerInstance is a started container sandbox.
type containerInstance struct {
	backend   *containerBackend
	name      string
	endpoints Endpoints
	// remove tells Stop whether to delete the container. False under --keep.
	remove bool
}

func (i *containerInstance) Endpoints() Endpoints { return i.endpoints }

func (i *containerInstance) Stop(ctx context.Context) error {
	if !i.remove {
		return nil
	}
	if err := i.backend.remove(ctx, i.name); err != nil {
		return fmt.Errorf("removing container %s: %w", i.name, err)
	}
	return nil
}

// Start creates the container, discovers its published ports and waits for
// sandboxd to report healthy.
func (b *containerBackend) Start(ctx context.Context, spec Spec) (Instance, error) {
	// One deadline for the whole startup, so Start cannot outlast --timeout.
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()

	if _, err := b.runner.run(ctx, b.engine, containerRunArgs(spec)...); err != nil {
		// A failed `run` does not prove the engine created nothing: it may
		// have accepted the request before the CLI lost its answer, and --rm
		// never fires while sandboxd keeps running.
		b.removeIfOurs(spec.Name)
		return nil, fmt.Errorf("starting sandbox container: %w", err)
	}

	instance := &containerInstance{
		backend: b,
		name:    spec.Name,
		remove:  spec.Remove,
	}

	// The container now exists, so every failure path below must tear it down
	// or the user is left with an orphan.
	endpoints, err := b.resolveEndpoints(ctx, spec)
	if err != nil {
		b.cleanup(instance.name)
		return nil, err
	}
	instance.endpoints = endpoints

	if err := waitHealthy(ctx, b.httpClient, endpoints.RESTAddr); err != nil {
		b.cleanup(instance.name)
		return nil, err
	}
	return instance, nil
}

// remove deletes the container. --force because sandboxd outlives the one
// command we sent it, so the container is still running; --volumes stops the
// image's anonymous volumes accumulating across runs.
func (b *containerBackend) remove(ctx context.Context, name string) error {
	_, err := b.runner.run(ctx, b.engine, "rm", "--force", "--volumes", name)
	// --rm may already have reaped it, which is the state we wanted.
	if err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

// cleanup removes a container created by a Start that then failed. It ignores
// spec.Remove: the sandbox was never handed to the caller, so leaving it
// behind would just be a leak.
func (b *containerBackend) cleanup(name string) {
	// A fresh context: the caller's is often already cancelled, which is
	// frequently the very reason Start is unwinding.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := b.remove(ctx, name); err != nil {
		fmt.Fprintf(b.progress, "agtsbx: warning: could not remove container %s: %v\n", name, err)
	}
}

// removeIfOurs cleans up after an ambiguous `run`, but only if the container
// carries our label, so a name that collided with the user's own container
// leaves theirs alone.
func (b *containerBackend) removeIfOurs(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	out, err := b.runner.run(ctx, b.engine, "inspect", "--format",
		fmt.Sprintf("{{index .Config.Labels %q}}", managedByLabel), name)
	if err != nil || strings.TrimSpace(string(out)) != managedByValue {
		return
	}
	if err := b.remove(ctx, name); err != nil {
		fmt.Fprintf(b.progress, "agtsbx: warning: could not remove container %s: %v\n", name, err)
	}
}

// isNoSuchContainer reports whether err is the engine complaining that the
// container is already gone. Both engines say so only in prose.
func isNoSuchContainer(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such object")
}

// resolveEndpoints asks the engine which host ports it bound to sandboxd's
// ports.
func (b *containerBackend) resolveEndpoints(ctx context.Context, spec Spec) (Endpoints, error) {
	grpcAddr, err := b.publishedAddr(ctx, spec.Name, spec.GRPCPort)
	if err != nil {
		return Endpoints{}, err
	}
	restAddr, err := b.publishedAddr(ctx, spec.Name, spec.RESTPort)
	if err != nil {
		return Endpoints{}, err
	}
	return Endpoints{GRPCAddr: grpcAddr, RESTAddr: restAddr}, nil
}

// publishedAddr returns the host address bound to containerPort.
func (b *containerBackend) publishedAddr(ctx context.Context, name string, containerPort int) (string, error) {
	out, err := b.runner.run(ctx, b.engine, "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return "", fmt.Errorf("looking up published port for container port %d: %w", containerPort, err)
	}
	addr, err := parsePublishedPort(string(out))
	if err != nil {
		return "", fmt.Errorf("container port %d: %w", containerPort, err)
	}
	return addr, nil
}

// containerRunArgs builds the engine's `run` argument list for spec.
//
// The host port is left empty ("127.0.0.1::9090") so the engine allocates a
// free one; choosing one ourselves would race concurrent invocations. The
// bind address is pinned to loopback because sandboxd authenticates nothing
// (KEP-539.2 places containment in the network layer), so its control plane
// must not be reachable off-host as the engine default would allow.
//
// Capabilities and no-new-privileges mirror the securityContext in
// buildSandbox. The user is not forced here: local engines have no
// runAsNonRoot equivalent, so on this path the image decides.
func containerRunArgs(spec Spec) []string {
	args := []string{"run", "--detach", "--name", spec.Name}
	if spec.Remove {
		// Belt and braces alongside Stop: if agtsbx is killed before it can
		// clean up, the engine still reaps the container once it stops.
		args = append(args, "--rm")
	}
	return append(args,
		"--label", managedByLabel+"="+managedByValue,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--publish", fmt.Sprintf("127.0.0.1::%d", spec.GRPCPort),
		"--publish", fmt.Sprintf("127.0.0.1::%d", spec.RESTPort),
		spec.Image,
	)
}

// parsePublishedPort extracts a dialable address from `docker port` output.
// The engine prints one binding per line, and may list both IPv4 and IPv6 for
// one container port; either reaches the same listener, so the first wins.
func parsePublishedPort(out string) (string, error) {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, port, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		if _, err := strconv.Atoi(port); err != nil {
			continue
		}
		// A wildcard bind address is not dialable as printed.
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port), nil
	}
	return "", fmt.Errorf("no published host port found in engine output %q", strings.TrimSpace(out))
}
