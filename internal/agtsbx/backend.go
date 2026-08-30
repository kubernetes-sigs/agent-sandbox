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

// Package agtsbx implements the agtsbx command-line tool: a docker-like front
// end for running one-shot sandboxes.
//
// Every backend converges on the KEP-539.2 sandboxd contract, so a backend
// only has to answer "where do I dial sandboxd?".
package agtsbx

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"time"
)

// Runtime names accepted by --runtime.
const (
	// RuntimeAuto probes the host and picks the first usable backend.
	RuntimeAuto = "auto"
	// RuntimeDocker runs the sandbox as a local Docker container.
	RuntimeDocker = "docker"
	// RuntimePodman runs the sandbox as a local Podman container.
	RuntimePodman = "podman"
	// RuntimeKubernetes runs the sandbox as a Sandbox object in a cluster.
	RuntimeKubernetes = "kubernetes"
)

// autoRuntimePreference is the probe order used by RuntimeAuto. Local engines
// come first because they need no cluster, kubeconfig or controller install.
var autoRuntimePreference = []string{RuntimeDocker, RuntimePodman, RuntimeKubernetes}

// knownRuntimes is every value --runtime accepts, in help-text order.
var knownRuntimes = []string{RuntimeAuto, RuntimeDocker, RuntimePodman, RuntimeKubernetes}

// Default sandboxd listener ports inside the sandbox (KEP-539.2).
const (
	defaultGRPCPort = 9090
	defaultRESTPort = 8080
)

// cleanupTimeout bounds teardown, which runs on paths where the caller's
// context is already cancelled and so needs a deadline of its own.
const cleanupTimeout = 30 * time.Second

// managedByValue marks the sandboxes agtsbx created, so failed-start cleanup
// never removes an object that merely shares the requested name.
const managedByValue = "agtsbx"

// Endpoints locates a running sandbox's sandboxd listeners, as addresses
// reachable from the caller: published loopback ports for a container
// backend, the local ends of a port-forward for Kubernetes.
type Endpoints struct {
	// GRPCAddr is the "host:port" of sandboxd's ProcessService.
	GRPCAddr string
	// RESTAddr is the "host:port" of sandboxd's Filesystem & Runtime REST API.
	RESTAddr string
}

// Spec describes the sandbox to start. It holds only what every backend can
// honour; backend-specific knobs live in backendOptions.
//
// It carries no environment: sandboxd takes the command's variables per
// request, so keeping them out of here keeps credentials out of the engine
// argv and out of the Sandbox object.
type Spec struct {
	// Image must start sandboxd as its entrypoint.
	Image string
	// Name identifies the sandbox to the backend (container name, Sandbox
	// object name).
	Name string
	// GRPCPort and RESTPort are sandboxd's ports inside the sandbox.
	GRPCPort int
	RESTPort int
	// Remove requests that the sandbox be torn down once the command exits.
	Remove bool
}

// Instance is a sandbox that has been started and is reachable.
type Instance interface {
	// Endpoints reports where to reach sandboxd.
	Endpoints() Endpoints
	// Stop releases the sandbox and any local resources held for it. It is
	// safe to call more than once, and takes its own context because the
	// caller's is usually cancelled by the time teardown runs.
	Stop(ctx context.Context) error
}

// Backend starts sandboxes on one particular runtime.
type Backend interface {
	// Name is the --runtime value that selects this backend.
	Name() string
	// Start creates the sandbox and returns once sandboxd is reachable and
	// healthy. On error it cleans up whatever it already created.
	Start(ctx context.Context, spec Spec) (Instance, error)
}

// lookPathFunc mirrors exec.LookPath so runtime selection can be tested
// without depending on what happens to be installed on the machine.
type lookPathFunc func(file string) (string, error)

// selectRuntime resolves the --runtime value into a concrete backend name.
// An explicit choice is never silently downgraded to a different engine; only
// RuntimeAuto probes.
func selectRuntime(requested string, lookPath lookPathFunc) (string, error) {
	switch requested {
	case RuntimeDocker, RuntimePodman:
		if _, err := lookPath(requested); err != nil {
			return "", fmt.Errorf("runtime %q selected but the %q executable was not found in PATH: %w", requested, requested, err)
		}
		return requested, nil
	case RuntimeKubernetes:
		return RuntimeKubernetes, nil
	case RuntimeAuto:
		for _, candidate := range autoRuntimePreference {
			// Kubernetes is the terminal fallback: nothing to probe for, and
			// its own kubeconfig error beats a generic "no runtime found".
			if candidate == RuntimeKubernetes {
				return RuntimeKubernetes, nil
			}
			if _, err := lookPath(candidate); err == nil {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("no usable runtime found")
	default:
		return "", fmt.Errorf("unknown runtime %q, expected one of %v", requested, knownRuntimes)
	}
}

// newBackend constructs the backend named by runtime, which must already have
// been resolved by selectRuntime.
func newBackend(runtime string, opts backendOptions) (Backend, error) {
	switch runtime {
	case RuntimeDocker, RuntimePodman:
		return newContainerBackend(runtime, opts), nil
	case RuntimeKubernetes:
		return newKubeBackend(opts)
	default:
		return nil, fmt.Errorf("unknown runtime %q, expected one of %v", runtime, knownRuntimes)
	}
}

// backendOptions carries the knobs only some backends can act on.
type backendOptions struct {
	// Namespace is where the Kubernetes backend creates the Sandbox.
	Namespace string
	// ReadyTimeout bounds the whole of Start.
	ReadyTimeout time.Duration
	// Stderr receives progress messages, keeping the sandboxed command's
	// stdout pipeable.
	Stderr writerFunc
}

// writerFunc is the minimal sink the backends need for progress output.
type writerFunc interface {
	Write(p []byte) (int, error)
}

// isKnownRuntime reports whether name is an accepted --runtime value.
func isKnownRuntime(name string) bool {
	return slices.Contains(knownRuntimes, name)
}

// defaultLookPath is the production lookPathFunc.
var defaultLookPath lookPathFunc = exec.LookPath
