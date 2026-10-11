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

package sandbox

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
)

func newRealProcessSandbox(t *testing.T) (*Sandbox, *processmanager.ProcessRegistry) {
	t.Helper()
	registry := processmanager.NewProcessRegistry()
	sb := newReadySandboxdTestSandbox("http://unused")
	startFakeProcessServer(t, sb, server.NewProcessServer(t.TempDir(), registry, 0, logr.Discard()))
	t.Cleanup(func() { require.NoError(t, sb.Disconnect(context.Background())) })
	return sb, registry
}

func TestProcessHandle_RealIncrementalOutput(t *testing.T) {
	sb, registry := newRealProcessSandbox(t)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "printf early; while [ ! -f released ]; do sleep 0.01; done; printf late; printf problem >&2; exit 7")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	event, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStdout, event.Type)
	require.Equal(t, []byte("early"), event.Data)
	proc, ok := registry.Get(handle.ProcessID())
	require.True(t, ok, "command must still be registered before its gate is released")
	require.False(t, proc.IsCompleted(), "output must arrive before command completion")
	_, err = sb.Run(ctx, "touch released")
	require.NoError(t, err)
	var stdout, stderr []byte
	var exitCode int
	for {
		event, err := handle.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch event.Type {
		case ProcessEventStdout:
			stdout = append(stdout, event.Data...)
		case ProcessEventStderr:
			stderr = append(stderr, event.Data...)
		case ProcessEventExit:
			exitCode = event.ExitCode
		}
	}
	require.Equal(t, "late", string(stdout))
	require.Equal(t, "problem", string(stderr))
	require.Equal(t, 7, exitCode)
}

func TestProcessHandle_RealSignalAndExit(t *testing.T) {
	sb, _ := newRealProcessSandbox(t)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "trap 'printf stopped; exit 23' TERM; printf ready; while :; do sleep 0.01; done")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	event, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStdout, event.Type)
	require.Equal(t, []byte("ready"), event.Data, "signal only after the trap is installed")
	require.NoError(t, handle.SendSignal(ctx, SignalSIGTERM))
	var output []byte
	exitCode := 0
	for {
		event, err := handle.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch event.Type {
		case ProcessEventStdout:
			output = append(output, event.Data...)
		case ProcessEventExit:
			exitCode = event.ExitCode
		}
	}
	require.Equal(t, "stopped", string(output))
	require.Equal(t, 23, exitCode)
}

func TestProcessHandle_RealShortCommandWithEnvAndWorkingDir(t *testing.T) {
	sb, _ := newRealProcessSandbox(t)
	ctx := processTestContext(t)
	_, err := sb.Run(ctx, "mkdir work")
	require.NoError(t, err)
	handle, err := sb.Commands().Start(ctx, `printf '%s:' "$STREAM_ENV"; basename "$PWD"; exit 11`,
		WithEnv(map[string]string{"STREAM_ENV": "original"}),
		WithEnv(map[string]string{"STREAM_ENV": "overridden"}), WithWorkingDir("work"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	var output []byte
	var exitCode int
	for {
		event, err := handle.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch event.Type {
		case ProcessEventStdout:
			output = append(output, event.Data...)
		case ProcessEventExit:
			exitCode = event.ExitCode
		}
	}
	require.Equal(t, "overridden:work\n", string(output))
	require.Equal(t, 11, exitCode)
}

func TestProcessHandle_RealCancelAndCloseReapProcess(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			sb, registry := newRealProcessSandbox(t)
			ctx, cancel := context.WithCancel(processTestContext(t))
			defer cancel()
			handle, err := sb.Commands().Start(ctx, "printf ready; exec sleep 30")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			event, err := handle.Recv()
			require.NoError(t, err)
			require.Equal(t, []byte("ready"), event.Data)
			proc, ok := registry.Get(handle.ProcessID())
			require.True(t, ok)
			if action == "cancel" {
				cancel()
			} else {
				require.NoError(t, handle.Close())
			}
			select {
			case <-proc.Done:
				// Done is published after cmd.Wait has reaped the direct child.
				require.NotNil(t, proc.Cmd.ProcessState)
				require.True(t, proc.IsCompleted())
			case <-time.After(2 * time.Second):
				t.Fatal("cancelling Start must terminate the managed child")
			}
		})
	}
}

func TestProcessHandle_RealCloseIsolatesSharedConnection(t *testing.T) {
	sb, registry := newRealProcessSandbox(t)
	ctx := processTestContext(t)
	first, err := sb.Commands().Start(ctx, "exec sleep 30")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	firstProc, ok := registry.Get(first.ProcessID())
	require.True(t, ok)
	second, err := sb.Commands().Start(ctx, "printf alive; while [ ! -f released ]; do sleep 0.01; done; exit 0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	event, err := second.Recv()
	require.NoError(t, err)
	require.Equal(t, []byte("alive"), event.Data)
	require.NoError(t, first.Close())
	select {
	case <-firstProc.Done:
	case <-ctx.Done():
		t.Fatal("closed session was not reaped")
	}
	result, err := sb.Run(ctx, "printf connected; touch released")
	require.NoError(t, err)
	require.Equal(t, "connected", result.Stdout)
	event, err = second.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventExit, event.Type)
	require.Zero(t, event.ExitCode)
	_, err = second.Recv()
	require.ErrorIs(t, err, io.EOF)
}
