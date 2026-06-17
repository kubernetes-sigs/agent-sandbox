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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// Sleep until the parent kills us. We use a long sleep rather than an empty
	// `select{}` because the latter trips Go's deadlock detector (exit code 2),
	// which races the kill and makes the kill-signal assertion flaky.
	time.Sleep(time.Hour)
}

func TestIsHeartbeatProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping /proc based test on non-linux platform")
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "heartbeat", "--name", "test-resource")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// Let the OS populate /proc/<pid>/cmdline
	time.Sleep(50 * time.Millisecond)

	// Verify isHeartbeatProcess returns true for our helper process
	if !isHeartbeatProcess(cmd.Process.Pid, "test-resource") {
		t.Errorf("expected isHeartbeatProcess to return true, but got false")
	}

	// Verify isHeartbeatProcess returns false for a different resource name
	if isHeartbeatProcess(cmd.Process.Pid, "other-resource") {
		t.Errorf("expected isHeartbeatProcess to return false for different resource name, but got true")
	}

	// Spawn a standard sleep command which does not have "heartbeat" arguments
	dummy := exec.Command("sleep", "10")
	if err := dummy.Start(); err != nil {
		t.Fatalf("failed to start dummy process: %v", err)
	}
	defer func() {
		_ = dummy.Process.Kill()
		_ = dummy.Wait()
	}()

	if isHeartbeatProcess(dummy.Process.Pid, "test-resource") {
		t.Errorf("expected isHeartbeatProcess to return false for generic process, but got true")
	}
}

func TestConcurrentStateUpdates(t *testing.T) {
	// Create a temporary home directory to sandbox the state file.
	tmpHome, err := os.MkdirTemp("", "resourcectl-test-home-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpHome)

	t.Setenv("HOME", tmpHome)

	// Concurrently append a distinct resource from many goroutines. Without the
	// state file lock these read-modify-write cycles would race and lose
	// entries; with it, every entry must survive.
	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- updateState(func(state *State) error {
				// Widen the read-modify-write window so a missing lock would
				// reliably drop entries.
				time.Sleep(time.Millisecond)
				state.BoskosResources = append(state.BoskosResources, BoskosResource{
					Name: fmt.Sprintf("resource-%d", i),
				})
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("updateState failed: %v", err)
		}
	}

	state, err := readState()
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	if len(state.BoskosResources) != n {
		t.Errorf("expected %d resources after concurrent updates, but got %d", n, len(state.BoskosResources))
	}

	seen := make(map[string]int, len(state.BoskosResources))
	for _, resource := range state.BoskosResources {
		seen[resource.Name]++
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("resource-%d", i)
		if seen[name] != 1 {
			t.Fatalf("expected exactly one %q entry, got %d", name, seen[name])
		}
	}
}

func TestRunCleanup(t *testing.T) {
	// Create temporary home directory to sandbox state file
	tmpHome, err := os.MkdirTemp("", "resourcectl-test-home-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpHome)

	// Override HOME
	t.Setenv("HOME", tmpHome)

	// Start a mock Boskos server
	mockBoskos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Just return 200 OK
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status": "ok"}`)
	}))
	defer mockBoskos.Close()

	// Override BOSKOS_HOST
	origBoskosHost := os.Getenv("BOSKOS_HOST")
	os.Setenv("BOSKOS_HOST", mockBoskos.URL)
	defer os.Setenv("BOSKOS_HOST", origBoskosHost)

	// 1. Spawn a matching heartbeat process that we want to kill.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "heartbeat", "--name", "matching-resource")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start matching helper process: %v", err)
	}
	var cmdWaited bool
	defer func() {
		_ = cmd.Process.Kill()
		if !cmdWaited {
			_ = cmd.Wait()
		}
	}()

	// 2. Spawn a recycled/innocent process that should NOT be killed.
	// We'll use a simple sleep command as the innocent process.
	innocentCmd := exec.Command("sleep", "30")
	if err := innocentCmd.Start(); err != nil {
		t.Fatalf("failed to start innocent process: %v", err)
	}
	defer func() {
		_ = innocentCmd.Process.Kill()
		_ = innocentCmd.Wait()
	}()

	// Let the OS populate /proc/<pid>/cmdline for both processes
	time.Sleep(50 * time.Millisecond)

	// 3. Generate a dead PID (start and exit)
	deadCmd := exec.Command("true")
	if err := deadCmd.Run(); err != nil {
		t.Fatalf("failed to run dead process: %v", err)
	}
	deadPid := deadCmd.Process.Pid

	// Write initial state.json
	stateFile, err := stateFilePath()
	if err != nil {
		t.Fatalf("failed to get state file path: %v", err)
	}

	initialState := State{
		BoskosResources: []BoskosResource{
			{
				Name:         "matching-resource",
				Type:         "gce-project",
				HeartbeatPID: cmd.Process.Pid,
			},
			{
				Name:         "innocent-resource",
				Type:         "gce-project",
				HeartbeatPID: innocentCmd.Process.Pid,
			},
			{
				Name:         "dead-resource",
				Type:         "gce-project",
				HeartbeatPID: deadPid,
			},
		},
	}

	data, err := json.MarshalIndent(initialState, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal initial state: %v", err)
	}
	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		t.Fatalf("failed to write initial state file: %v", err)
	}

	// Run cleanup!
	ctx := context.Background()
	err = runCleanup(ctx)
	if err != nil {
		t.Fatalf("runCleanup failed: %v", err)
	}

	// 4. Verify results:
	// - The matching-resource heartbeat process should be killed.
	// - The innocent-resource process should STILL be running (not killed).
	// - The state file should be completely empty of resources (since all released successfully).

	// Verify matching heartbeat is killed.
	// Since the child process was killed by SIGKILL, wait should return an error indicating it was signaled.
	err = cmd.Wait()
	cmdWaited = true
	if err == nil {
		t.Errorf("expected matching heartbeat process to be killed, but it exited cleanly")
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status := exitErr.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Errorf("expected process to be killed by SIGKILL, but got exit status: %v", err)
			}
		} else {
			t.Errorf("unexpected error waiting for process: %v", err)
		}
	}

	// Verify innocent process is NOT killed (on Linux only, where verification is active).
	if runtime.GOOS == "linux" {
		process, err := os.FindProcess(innocentCmd.Process.Pid)
		if err == nil {
			err = process.Signal(syscall.Signal(0))
			if err != nil {
				t.Errorf("expected innocent process to still be running, but got error: %v", err)
			}
		}
	}

	// Verify state file is empty.
	state, err := readState()
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	if len(state.BoskosResources) != 0 {
		t.Errorf("expected 0 resources left in state.json, but got %d: %+v", len(state.BoskosResources), state.BoskosResources)
	}
}
