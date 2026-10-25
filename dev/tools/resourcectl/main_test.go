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

// TestMain lets runGet's `exec.Command(os.Args[0], "heartbeat", ...)` execute
// the real heartbeat CLI entrypoint inside the test binary at a fast interval.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "heartbeat" {
		heartbeatInterval = 20 * time.Millisecond
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

// TestRunHeartbeatRefreshesLeasePeriodically pins the one job the heartbeat
// exists for: repeated /update calls that keep the Boskos lease ahead of the
// reaper, surviving transient failures. The kOps lanes lost their projects
// to the Janitor 30 minutes after acquire because nothing asserted this.
func TestRunHeartbeatRefreshesLeasePeriodically(t *testing.T) {
	origInterval := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = origInterval }()

	var mu sync.Mutex
	var updates int
	var badRequests []string
	mockBoskos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		if r.Method != http.MethodPost || r.URL.Path != "/update" ||
			q.Get("name") != "test-resource" || q.Get("owner") != "test-owner" || q.Get("state") != "busy" {
			badRequests = append(badRequests, r.Method+" "+r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		updates++
		// Simulate Boskos's informer cache not yet reflecting /acquire on the
		// first refresh (401 owner mismatch with empty owner), plus a transient
		// 500 on the second, to prove neither ends the heartbeat loop.
		switch updates {
		case 1:
			http.Error(w, "Update failed: test-resource - busy (test-owner): owner mismatch request by test-owner, currently owned by ", http.StatusUnauthorized)
			return
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mockBoskos.Close()
	t.Setenv("BOSKOS_HOST", mockBoskos.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := runHeartbeat(ctx, "test-resource", "test-owner")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runHeartbeat returned %v, want it to run until the context expires", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(badRequests) > 0 {
		t.Errorf("unexpected Boskos requests: %v", badRequests)
	}
	// Roughly 25 ticks at a 20ms interval; only a lower bound is meaningful
	// on a loaded CI machine.
	if updates < 5 {
		t.Errorf("got %d /update calls in 500ms at a 20ms interval, want at least 5", updates)
	}
}

// TestRunGetRefreshesLeaseUntilCleanup exercises the full get -> detached
// heartbeat subprocess -> cleanup lifecycle against a mock Boskos server,
// including an initial 401 owner-mismatch response while Boskos's informer
// cache catches up after /acquire.
func TestRunGetRefreshesLeaseUntilCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("JOB_NAME", "test-job")
	t.Setenv("BUILD_ID", "12345")
	const wantOwner = "resourcectl-test-job-12345"
	const wantResource = "test-project"

	var mu sync.Mutex
	var acquiredAt time.Time
	var acquires, totalUpdates, okUpdates, releases int
	var badRequests []string

	mockBoskos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/acquire" &&
			q.Get("type") == "gce-project" && q.Get("state") == "free" &&
			q.Get("dest") == "busy" && q.Get("owner") == wantOwner:
			acquires++
			acquiredAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"name":%q}`, wantResource)

		case r.Method == http.MethodPost && r.URL.Path == "/update" &&
			q.Get("name") == wantResource && q.Get("state") == "busy" &&
			q.Get("owner") == wantOwner:
			totalUpdates++
			// Real Boskos serves /update reads from a controller-runtime informer
			// cache that briefly lags /acquire (< 20ms child tick interval here)
			// and rejects immediate updates with 401 and an empty current owner.
			if time.Since(acquiredAt) < 15*time.Millisecond {
				http.Error(w, fmt.Sprintf("Update failed: %s - busy (%s): owner mismatch request by %s, currently owned by ", wantResource, wantOwner, wantOwner), http.StatusUnauthorized)
				return
			}
			okUpdates++
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/release" &&
			q.Get("name") == wantResource && q.Get("state") == "busy" &&
			q.Get("dest") == "free" && q.Get("owner") == wantOwner:
			releases++
			w.WriteHeader(http.StatusOK)

		default:
			badRequests = append(badRequests, r.Method+" "+r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(mockBoskos.Close)
	t.Setenv("BOSKOS_HOST", mockBoskos.URL)

	var heartbeatPID int
	t.Cleanup(func() {
		if heartbeatPID != 0 {
			if p, err := os.FindProcess(heartbeatPID); err == nil {
				_ = p.Kill()
				_, _ = p.Wait()
			}
		}
	})

	ctx := context.Background()
	if err := runGet(ctx, "gce-project", "project-main"); err != nil {
		t.Fatalf("runGet failed: %v", err)
	}

	state, err := readState()
	if err != nil {
		t.Fatalf("readState failed: %v", err)
	}
	if len(state.BoskosResources) != 1 {
		t.Fatalf("expected 1 resource in state, got %d: %+v", len(state.BoskosResources), state.BoskosResources)
	}
	got := state.BoskosResources[0]
	heartbeatPID = got.HeartbeatPID
	if got.Name != wantResource || got.Owner != wantOwner || got.Key != "project-main" || heartbeatPID == 0 {
		t.Fatalf("unexpected persisted resource state: %+v", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := okUpdates
		total := totalUpdates
		mu.Unlock()
		if ok >= 3 {
			break
		}
		if runtime.GOOS == "linux" && !isHeartbeatProcess(heartbeatPID, wantResource) {
			t.Fatalf("heartbeat process (pid %d) exited before cleanup after %d /update attempt(s) (%d succeeded)", heartbeatPID, total, ok)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for >= 3 successful /update calls; got %d total (%d succeeded)", total, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if runtime.GOOS == "linux" && !isHeartbeatProcess(heartbeatPID, wantResource) {
		t.Fatalf("expected heartbeat process (pid %d) to still be running before cleanup", heartbeatPID)
	}

	if err := runCleanup(ctx); err != nil {
		t.Fatalf("runCleanup failed: %v", err)
	}
	if p, err := os.FindProcess(heartbeatPID); err == nil {
		_, _ = p.Wait()
	}
	heartbeatPID = 0

	mu.Lock()
	defer mu.Unlock()
	if len(badRequests) > 0 {
		t.Errorf("unexpected Boskos requests: %v", badRequests)
	}
	if acquires != 1 {
		t.Errorf("got %d /acquire calls, want 1", acquires)
	}
	if releases != 1 {
		t.Errorf("got %d /release calls, want 1", releases)
	}

	finalState, err := readState()
	if err != nil {
		t.Fatalf("readState after cleanup failed: %v", err)
	}
	if len(finalState.BoskosResources) != 0 {
		t.Errorf("expected 0 resources after cleanup, got %+v", finalState.BoskosResources)
	}
}
