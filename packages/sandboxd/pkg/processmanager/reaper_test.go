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

package processmanager

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestReaperLifecycle(t *testing.T) {
	// Safe operations on nil reaper
	var nilReaper *Reaper
	nilReaper.Start()
	nilReaper.Stop()
	nilReaper.Track(123)
	nilReaper.Untrack(123)
	nilReaper.Reap()
	nilReaper.SetWaitTimeout(time.Second)
	require.Equal(t, int64(0), nilReaper.ReapedOrphans())

	cmd := exec.Command("true")
	code, err := nilReaper.WaitResult(cmd, nil)
	require.NoError(t, err)
	require.Equal(t, int32(0), code)

	// Idempotent Start and Stop
	r := NewReaper(logr.Discard())
	r.Start()
	r.Start() // second start is no-op
	r.Stop()
	r.Stop() // second stop is no-op
}

func TestReaperUntrackedChildReaped(t *testing.T) {
	r := StartReaper(logr.Discard())
	defer r.Stop()

	// Spawn an untracked child process that exits immediately.
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	// Wait for the background reaper to reap the untracked child.
	require.Eventually(t, func() bool {
		return r.ReapedOrphans() > 0
	}, 3*time.Second, 10*time.Millisecond)

	// Calling cmd.Wait() should return ECHILD because the reaper already reaped it.
	waitErr := cmd.Wait()
	require.Error(t, waitErr)
	require.ErrorIs(t, waitErr, syscall.ECHILD, "expected ECHILD from already-reaped child, got: %v", waitErr)

	// Kernel process table should no longer contain this PID.
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	require.Equal(t, -1, wpid)
	require.ErrorIs(t, err, syscall.ECHILD)
}

func TestReaperTrackedCommandNormalExit(t *testing.T) {
	r := StartReaper(logr.Discard())
	defer r.Stop()

	tests := []struct {
		name         string
		script       string
		wantExitCode int32
	}{
		{
			name:         "exit 0",
			script:       "exit 0",
			wantExitCode: 0,
		},
		{
			name:         "exit 42",
			script:       "exit 42",
			wantExitCode: 42,
		},
		{
			name:         "exit 1",
			script:       "exit 1",
			wantExitCode: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", tt.script)
			require.NoError(t, cmd.Start())
			pid := cmd.Process.Pid

			r.Track(pid)
			defer r.Untrack(pid)

			waitErr := cmd.Wait()
			exitCode, err := r.WaitResult(cmd, waitErr)
			require.NoError(t, err)
			require.Equal(t, tt.wantExitCode, exitCode)
		})
	}
}

func TestReaperTrackedCommandReapedFirst(t *testing.T) {
	r := StartReaper(logr.Discard())
	defer r.Stop()

	// Run multiple iterations where the child exits and sleeps so the background
	// reaper collects it before cmd.Wait() is called.
	for range 5 {
		cmd := exec.Command("sh", "-c", "exit 42")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid

		r.Track(pid)

		// Give the child and reaper time to reap the process before calling cmd.Wait().
		time.Sleep(30 * time.Millisecond)

		waitErr := cmd.Wait()
		exitCode, err := r.WaitResult(cmd, waitErr)
		r.Untrack(pid)

		require.NoError(t, err)
		require.Equal(t, int32(42), exitCode, "exit code must be preserved when reaper reaped first")
	}
}

func TestReaperTrackedCommandSignaled(t *testing.T) {
	r := StartReaper(logr.Discard())
	defer r.Stop()

	cmd := exec.Command("sleep", "10")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	r.Track(pid)
	defer r.Untrack(pid)

	// Send SIGKILL
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))

	waitErr := cmd.Wait()
	exitCode, err := r.WaitResult(cmd, waitErr)
	require.NoError(t, err)
	// SIGKILL (9) -> 128 + 9 = 137
	require.Equal(t, int32(128+int(syscall.SIGKILL)), exitCode)
}

func TestReaperConcurrentTrackedAndUntracked(t *testing.T) {
	r := StartReaper(logr.Discard())
	defer r.Stop()

	const workers = 8
	const perWorker = 5

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// Worker group 1: tracked commands with known exit codes
	for w := range workers {
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				expectedCode := int32((w + i) % 10)
				cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(int(expectedCode)))
				if err := cmd.Start(); err != nil {
					t.Errorf("cmd.Start: %v", err)
					return
				}
				pid := cmd.Process.Pid
				r.Track(pid)

				waitErr := cmd.Wait()
				exitCode, err := r.WaitResult(cmd, waitErr)
				r.Untrack(pid)

				if err != nil {
					t.Errorf("WaitResult unexpected error: %v", err)
					return
				}
				if exitCode != expectedCode {
					t.Errorf("expected exit code %d, got %d", expectedCode, exitCode)
					return
				}
			}
		}(w)
	}

	// Worker group 2: untracked commands (simulating orphaned grandchildren)
	for range workers {
		go func() {
			defer wg.Done()
			for range perWorker {
				cmd := exec.Command("sh", "-c", "exit 0")
				if err := cmd.Start(); err != nil {
					t.Errorf("untracked cmd.Start: %v", err)
					return
				}
				// Do not track, do not wait - the background reaper must reap them!
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// Verify the reaper reaped untracked child processes
	require.Eventually(t, func() bool {
		return r.ReapedOrphans() >= int64(workers*perWorker)
	}, 5*time.Second, 20*time.Millisecond)
}
