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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner records engine invocations and replays canned responses, so the
// container backend can be exercised with no engine installed.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// responses maps the first argument (the subcommand) to its stdout.
	responses map[string]string
	// errs maps a subcommand to an error to return instead.
	errs map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 0 {
		return nil, nil
	}
	if err, ok := f.errs[args[0]]; ok {
		return nil, err
	}
	return []byte(f.responses[args[0]]), nil
}

// callsFor returns every recorded invocation whose subcommand is sub.
func (f *fakeRunner) callsFor(sub string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, call := range f.calls {
		if len(call) > 1 && call[1] == sub {
			out = append(out, call)
		}
	}
	return out
}

func TestContainerRunArgs(t *testing.T) {
	t.Run("publishes both sandboxd ports on loopback with an ephemeral host port", func(t *testing.T) {
		args := containerRunArgs(Spec{
			Image:    "sandboxd:latest",
			Name:     "agtsbx-abc",
			GRPCPort: 9090,
			RESTPort: 8080,
		})

		// Loopback pinning is a security property: sandboxd authenticates
		// nothing, so its control plane must not be published to the network.
		joined := strings.Join(args, " ")
		assert.Contains(t, joined, "--publish 127.0.0.1::9090")
		assert.Contains(t, joined, "--publish 127.0.0.1::8080")
		assert.NotContains(t, joined, "0.0.0.0")
	})

	t.Run("confines the container the way buildSandbox confines the pod", func(t *testing.T) {
		// A local sandbox runs the same untrusted code as a cluster one, so
		// it must not be the softer target of the two.
		args := containerRunArgs(Spec{Image: "sandboxd:latest", Name: "agtsbx-abc"})

		joined := strings.Join(args, " ")
		assert.Contains(t, joined, "--cap-drop ALL")
		assert.Contains(t, joined, "--security-opt no-new-privileges")
	})

	t.Run("runs detached with the image last", func(t *testing.T) {
		args := containerRunArgs(Spec{Image: "sandboxd:latest", Name: "agtsbx-abc"})

		assert.Equal(t, "run", args[0])
		assert.Contains(t, args, "--detach")
		// Everything after the image is the container's command, so the image
		// must terminate the argument list.
		assert.Equal(t, "sandboxd:latest", args[len(args)-1])
	})

	t.Run("keeps the environment off the engine argv", func(t *testing.T) {
		// The command's variables travel in the sandboxd request, so a
		// credential never appears in host ps output or docker inspect.
		args := containerRunArgs(Spec{Image: "sandboxd:latest", Name: "agtsbx-abc"})
		assert.NotContains(t, args, "--env")
		assert.NotContains(t, args, "-e")
	})

	t.Run("labels the container as ours", func(t *testing.T) {
		// Failed-start cleanup removes only labelled containers.
		args := containerRunArgs(Spec{Image: "i", Name: "n"})
		assert.Contains(t, strings.Join(args, " "), "--label "+managedByLabel+"="+managedByValue)
	})

	t.Run("adds --rm only when the sandbox is ephemeral", func(t *testing.T) {
		ephemeral := containerRunArgs(Spec{Image: "i", Name: "n", Remove: true})
		assert.Contains(t, ephemeral, "--rm")

		kept := containerRunArgs(Spec{Image: "i", Name: "n", Remove: false})
		assert.NotContains(t, kept, "--rm")
	})

	t.Run("does not set a container workdir", func(t *testing.T) {
		// --workdir belongs to the command, not the sandbox: it travels as
		// ProcessConfig.cwd. On the container it would move sandboxd's own cwd.
		args := containerRunArgs(Spec{Image: "i", Name: "n"})
		assert.NotContains(t, args, "--workdir")
		assert.NotContains(t, args, "-w")
	})
}

func TestParsePublishedPort(t *testing.T) {
	testCases := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name:   "loopback binding",
			output: "127.0.0.1:32768\n",
			want:   "127.0.0.1:32768",
		},
		{
			// Either of an IPv4/IPv6 pair reaches the same listener.
			name:   "first of several bindings wins",
			output: "127.0.0.1:32768\n[::1]:32769\n",
			want:   "127.0.0.1:32768",
		},
		{
			// A wildcard bind address is not dialable as printed.
			name:   "wildcard address is rewritten to loopback",
			output: "0.0.0.0:32768\n",
			want:   "127.0.0.1:32768",
		},
		{
			name:   "ipv6 wildcard is rewritten to loopback",
			output: "[::]:32768\n",
			want:   "127.0.0.1:32768",
		},
		{
			name:   "surrounding whitespace is tolerated",
			output: "  \n  127.0.0.1:32768  \n",
			want:   "127.0.0.1:32768",
		},
		{
			name:    "empty output is an error",
			output:  "\n  \n",
			wantErr: true,
		},
		{
			name:    "unparsable output is an error",
			output:  "no port mapping found",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePublishedPort(tc.output)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// newTestContainerBackend wires a container backend to a fake engine and a
// health endpoint served by the given handler.
func newTestContainerBackend(t *testing.T, runner commandRunner, handler http.HandlerFunc) (*containerBackend, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &containerBackend{
		engine:       "docker",
		runner:       runner,
		httpClient:   server.Client(),
		readyTimeout: 5 * time.Second,
		progress:     io.Discard,
	}, strings.TrimPrefix(server.URL, "http://")
}

func TestContainerBackendStart(t *testing.T) {
	t.Run("returns endpoints once sandboxd is healthy", func(t *testing.T) {
		runner := newFakeRunner()
		backend, addr := newTestContainerBackend(t, runner, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		// Both ports resolve to the stub server so the health probe reaches it.
		runner.responses["port"] = addr

		instance, err := backend.Start(context.Background(), Spec{
			Image: "sandboxd:latest", Name: "agtsbx-test", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.NoError(t, err)

		assert.Equal(t, addr, instance.Endpoints().RESTAddr)
		assert.Equal(t, addr, instance.Endpoints().GRPCAddr)
		// Each sandboxd port must be looked up separately.
		assert.Len(t, runner.callsFor("port"), 2)
	})

	t.Run("removes the container when the health probe never succeeds", func(t *testing.T) {
		runner := newFakeRunner()
		backend, addr := newTestContainerBackend(t, runner, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		runner.responses["port"] = addr
		backend.readyTimeout = 300 * time.Millisecond

		_, err := backend.Start(context.Background(), Spec{
			Image: "sandboxd:latest", Name: "agtsbx-test", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.Error(t, err)

		// The caller never received an Instance, so only Start can clean up.
		removals := runner.callsFor("rm")
		require.Len(t, removals, 1)
		assert.Contains(t, removals[0], "--force")
		assert.Contains(t, removals[0], "agtsbx-test")
	})

	t.Run("removes the container when port discovery fails", func(t *testing.T) {
		runner := newFakeRunner()
		backend, _ := newTestContainerBackend(t, runner, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		runner.errs["port"] = errors.New("no such container")

		_, err := backend.Start(context.Background(), Spec{
			Image: "sandboxd:latest", Name: "agtsbx-test", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.Error(t, err)
		assert.Len(t, runner.callsFor("rm"), 1)
	})

	t.Run("does not remove anything when the container never started", func(t *testing.T) {
		runner := newFakeRunner()
		backend, _ := newTestContainerBackend(t, runner, func(http.ResponseWriter, *http.Request) {})
		runner.errs["run"] = errors.New("image not found")

		_, err := backend.Start(context.Background(), Spec{
			Image: "missing:latest", Name: "agtsbx-test", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image not found")
		assert.Empty(t, runner.callsFor("rm"))
	})

	t.Run("removes a container the engine created despite failing the run", func(t *testing.T) {
		// The daemon can accept the request before the CLI loses its answer,
		// and --rm never fires while sandboxd keeps running.
		runner := newFakeRunner()
		backend, _ := newTestContainerBackend(t, runner, func(http.ResponseWriter, *http.Request) {})
		runner.errs["run"] = context.DeadlineExceeded
		runner.responses["inspect"] = managedByValue + "\n"

		_, err := backend.Start(context.Background(), Spec{
			Image: "sandboxd:latest", Name: "agtsbx-test", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.Error(t, err)
		require.Len(t, runner.callsFor("rm"), 1)
	})

	t.Run("leaves a container that is not ours after a failed run", func(t *testing.T) {
		// --name can collide with something the user owns.
		runner := newFakeRunner()
		backend, _ := newTestContainerBackend(t, runner, func(http.ResponseWriter, *http.Request) {})
		runner.errs["run"] = errors.New("container name already in use")
		runner.responses["inspect"] = "\n"

		_, err := backend.Start(context.Background(), Spec{
			Image: "sandboxd:latest", Name: "mybox", GRPCPort: 9090, RESTPort: 8080, Remove: true,
		})
		require.Error(t, err)
		assert.Empty(t, runner.callsFor("rm"))
	})
}

func TestContainerInstanceStop(t *testing.T) {
	t.Run("removes an ephemeral sandbox", func(t *testing.T) {
		runner := newFakeRunner()
		backend := &containerBackend{engine: "docker", runner: runner, progress: io.Discard}
		instance := &containerInstance{backend: backend, name: "agtsbx-test", remove: true}

		require.NoError(t, instance.Stop(context.Background()))

		// --force is required because sandboxd outlives the single command
		// agtsbx sent it, so the container is still running.
		removals := runner.callsFor("rm")
		require.Len(t, removals, 1)
		assert.Contains(t, removals[0], "--force")
		assert.Contains(t, removals[0], "--volumes")
	})

	t.Run("leaves a kept sandbox running", func(t *testing.T) {
		runner := newFakeRunner()
		backend := &containerBackend{engine: "docker", runner: runner, progress: io.Discard}
		instance := &containerInstance{backend: backend, name: "agtsbx-test", remove: false}

		require.NoError(t, instance.Stop(context.Background()))
		assert.Empty(t, runner.calls, "--keep must not tear the sandbox down")
	})

	t.Run("surfaces engine failures", func(t *testing.T) {
		runner := newFakeRunner()
		runner.errs["rm"] = errors.New("permission denied")
		backend := &containerBackend{engine: "docker", runner: runner, progress: io.Discard}
		instance := &containerInstance{backend: backend, name: "agtsbx-test", remove: true}

		err := instance.Stop(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
	})

	t.Run("treats an already-removed container as success", func(t *testing.T) {
		// --rm may have reaped it first; that is the state we wanted.
		runner := newFakeRunner()
		runner.errs["rm"] = errors.New("Error response from daemon: No such container: agtsbx-test")
		backend := &containerBackend{engine: "docker", runner: runner, progress: io.Discard}
		instance := &containerInstance{backend: backend, name: "agtsbx-test", remove: true}

		require.NoError(t, instance.Stop(context.Background()))
	})
}

func TestExecRunnerIncludesStderrInError(t *testing.T) {
	// A bare exit status tells the user nothing actionable.
	_, err := execRunner{}.run(context.Background(), "sh", "-c", "echo boom >&2; exit 1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestExecRunnerReturnsStdout(t *testing.T) {
	out, err := execRunner{}.run(context.Background(), "sh", "-c", fmt.Sprintf("echo %s", "127.0.0.1:32768"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:32768", strings.TrimSpace(string(out)))
}
