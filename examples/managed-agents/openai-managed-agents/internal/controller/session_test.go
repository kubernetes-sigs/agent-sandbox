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

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// fakeSandbox records lifecycle calls and lets tests script failures and
// block points.
type fakeSandbox struct {
	mu    sync.Mutex
	calls []string

	ensureErr    error
	startErr     error
	startBlock   chan struct{} // when non-nil, StartExecutor waits on it
	connectErr   error
	connectBlock chan struct{}
	settledState string
	settledErr   error
	stopErr      error
	suspendErr   error
	releaseErr   error
}

func (f *fakeSandbox) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeSandbox) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSandbox) countCalls(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeSandbox) EnsureReady(context.Context) error {
	f.record("ensure")
	return f.ensureErr
}

func (f *fakeSandbox) StartExecutor(_ context.Context, _ SessionEvent, attemptID int) error {
	f.record(fmt.Sprintf("start:%d", attemptID))
	if f.startBlock != nil {
		<-f.startBlock
	}
	return f.startErr
}

func (f *fakeSandbox) WaitConnected(_ context.Context, _ time.Duration) error {
	f.record("connect")
	if f.connectBlock != nil {
		<-f.connectBlock
	}
	return f.connectErr
}

func (f *fakeSandbox) WaitTurnSettled(context.Context) (string, error) {
	f.record("settle")
	if f.settledState == "" {
		return "completed", f.settledErr
	}
	return f.settledState, f.settledErr
}

func (f *fakeSandbox) StopExecutor(context.Context) error {
	f.record("stop")
	return f.stopErr
}

func (f *fakeSandbox) Suspend(context.Context) error {
	f.record("suspend")
	return f.suspendErr
}

func (f *fakeSandbox) Release(context.Context) error {
	f.record("release")
	return f.releaseErr
}

func testEvent(id string) SessionEvent {
	return SessionEvent{SessionID: id, EnvironmentID: "env_" + id, RemoteURL: "wss://" + id}
}

func newTestManager(ctx context.Context, sb sessionSandbox, cfg *SessionManagerConfig) (*SessionManager, *int) {
	factoryCalls := 0
	if cfg == nil {
		cfg = &SessionManagerConfig{}
	}
	cfg.Factory = func(string) sessionSandbox { factoryCalls++; return sb }
	cfg.Logger = logr.Discard()
	if cfg.RetryBase == 0 {
		cfg.RetryBase = time.Millisecond
	}
	if cfg.ProcessAttempts == 0 {
		cfg.ProcessAttempts = 3
	}
	return NewSessionManager(ctx, *cfg), &factoryCalls
}

func waitForCond(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestEventAndSuspendSerialize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{startBlock: make(chan struct{})}
	mgr, _ := newTestManager(ctx, sb, nil)

	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return sb.countCalls("start:1") == 1 }, "event not started")
	// A suspend request queued behind the in-flight event must not run yet.
	s := mgr.list()[0]
	if !s.requestSuspend(0) {
		t.Fatal("suspend request rejected")
	}
	time.Sleep(20 * time.Millisecond)
	if sb.countCalls("suspend") != 0 {
		t.Fatal("suspend ran while event in flight")
	}
	close(sb.startBlock)
	waitForCond(t, 2*time.Second, func() bool { return sb.countCalls("suspend") == 1 }, "suspend never ran")
	// The executor is stopped before the pod is suspended.
	calls := sb.callList()
	stopIdx, suspIdx := -1, -1
	for i, c := range calls {
		if c == "stop" && stopIdx == -1 {
			stopIdx = i
		}
		if c == "suspend" && suspIdx == -1 {
			suspIdx = i
		}
	}
	if stopIdx < 0 || suspIdx < 0 || stopIdx > suspIdx {
		t.Fatalf("stop must precede suspend: %v", calls)
	}
}

func TestDispatchDedupWhileActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{startBlock: make(chan struct{})}
	mgr, _ := newTestManager(ctx, sb, nil)
	defer close(sb.startBlock)

	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return sb.countCalls("start:1") == 1 }, "not started")
	// The reconciler re-delivers the same action every poll; the manager
	// must fold duplicates away instead of queueing them.
	if err := mgr.Dispatch(testEvent("s1")); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("dup dispatch err = %v, want ErrAlreadyActive", err)
	}
	s := mgr.list()[0]
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatalf("duplicate queued: pending=%d", pending)
	}
}

func TestSuspendBlockedByAPIActiveCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{}
	var active atomic.Bool
	active.Store(true)
	mgr, _ := newTestManager(ctx, sb, &SessionManagerConfig{
		SessionActiveCheck: func(context.Context, string) (bool, error) { return active.Load(), nil },
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return sb.countCalls("settle") == 1 }, "event not processed")
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")

	s := mgr.list()[0]
	if !s.requestSuspend(0) {
		t.Fatal("suspend request rejected")
	}
	time.Sleep(30 * time.Millisecond)
	if sb.countCalls("suspend") != 0 {
		t.Fatal("suspended despite active platform session")
	}
	// Once the platform reports inactive, the next request suspends.
	active.Store(false)
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "busy after first suspend attempt")
	if !s.requestSuspend(0) {
		t.Fatal("second suspend request rejected")
	}
	waitForCond(t, time.Second, func() bool { return sb.countCalls("suspend") == 1 }, "suspend never ran")
}

func TestSuspendBlockedByCheckError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{}
	mgr, _ := newTestManager(ctx, sb, &SessionManagerConfig{
		SessionActiveCheck: func(context.Context, string) (bool, error) {
			return false, errors.New("api down")
		},
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	s := mgr.list()[0]
	s.requestSuspend(0)
	time.Sleep(30 * time.Millisecond)
	if sb.countCalls("suspend") != 0 {
		t.Fatal("suspended despite failed state check (must be conservative)")
	}
}

func TestSuspendBlockedByRunnerActiveWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{
		// Uncertain start: the executor may be running a turn we never saw.
		startErr: newPostError(errors.New("transport"), true),
	}
	mgr, _ := newTestManager(ctx, sb, &SessionManagerConfig{
		ProcessAttempts:    1,
		RunnerActiveWindow: time.Hour,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	s := mgr.list()[0]
	if !s.runnerMayBeActive() {
		t.Fatal("uncertain post must open the runner-active window")
	}
	s.requestSuspend(0)
	time.Sleep(30 * time.Millisecond)
	if sb.countCalls("suspend") != 0 {
		t.Fatal("suspended inside runner-active window")
	}
}

func TestConflictAbandonsAndReclaims(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &fakeSandbox{startErr: fmt.Errorf("runner said: %w", errConflictBinding)}
	second := &fakeSandbox{}
	handles := []sessionSandbox{first, second}
	factoryCalls := 0
	mgr := NewSessionManager(ctx, SessionManagerConfig{
		Factory:         func(string) sessionSandbox { h := handles[factoryCalls]; factoryCalls++; return h },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 2*time.Second, func() bool { return second.countCalls("settle") == 1 }, "retry on fresh sandbox never completed")
	if first.countCalls("release") != 1 {
		t.Fatalf("conflicted sandbox not released: %v", first.callList())
	}
	if first.countCalls("stop") != 0 {
		t.Fatal("must not stop another session's executor")
	}
	if factoryCalls != 2 {
		t.Fatalf("factory calls = %d, want 2", factoryCalls)
	}
}

func TestConflictTwiceIsTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handles := []sessionSandbox{
		&fakeSandbox{startErr: errConflictBinding},
		&fakeSandbox{startErr: errConflictBinding},
		&fakeSandbox{},
	}
	factoryCalls := 0
	mgr := NewSessionManager(ctx, SessionManagerConfig{
		Factory:         func(string) sessionSandbox { h := handles[factoryCalls]; factoryCalls++; return h },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 5,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 2*time.Second, func() bool { return mgr.list()[0].isFailed() }, "not terminal after second conflict")
	if err := mgr.Dispatch(testEvent("s1")); !errors.Is(err, ErrExecutorFailed) {
		t.Fatalf("dispatch to failed session err = %v, want ErrExecutorFailed", err)
	}
}

func TestConnectTimeoutTwiceFailsTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{connectErr: errors.New("timeout")}
	mgr, _ := newTestManager(ctx, sb, &SessionManagerConfig{
		ProcessAttempts: 5,
		ConnectTimeout:  time.Millisecond,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 2*time.Second, func() bool { return mgr.list()[0].isFailed() }, "not terminal after two connect timeouts")
	if got := sb.countCalls("connect"); got != 2 {
		t.Fatalf("connect attempts = %d, want exactly 2", got)
	}
	if err := mgr.Dispatch(testEvent("s1")); !errors.Is(err, ErrExecutorFailed) {
		t.Fatalf("dispatch err = %v, want ErrExecutorFailed", err)
	}
}

func TestFailedTurnIsNotBridgeFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{settledState: "failed"}
	mgr, _ := newTestManager(ctx, sb, nil)
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	if mgr.list()[0].isFailed() {
		t.Fatal("a failed turn must not fail the session terminally")
	}
	// The executor stays connected: no stop between settle and idle.
	if sb.countCalls("stop") != 0 {
		t.Fatal("executor stopped after failed turn")
	}
}

func TestTryReserveReleaseBoundaries(t *testing.T) {
	now := time.Now()
	s := &sessionState{lastDone: now.Add(-time.Hour), activeWindow: time.Minute}
	if !s.tryReserveRelease(now, 30*time.Minute) {
		t.Fatal("idle past threshold must reserve")
	}
	s.unreserve()
	if s.tryReserveRelease(now, 2*time.Hour) {
		t.Fatal("idle under threshold must not reserve")
	}
	s.runnerActiveUntil = now.Add(time.Minute)
	if s.tryReserveRelease(now, 30*time.Minute) {
		t.Fatal("runner-active window must block release")
	}
	s.runnerActiveUntil = time.Time{}
	ev := testEvent("s1")
	s.inFlight = &ev
	if s.tryReserveRelease(now, 30*time.Minute) {
		t.Fatal("busy session must not reserve")
	}
}

func TestConnectFailureBudgetResetsAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{connectErr: errors.New("timeout")}
	mgr, _ := newTestManager(ctx, sb, &SessionManagerConfig{
		ProcessAttempts: 1,
		ConnectTimeout:  time.Millisecond,
	})
	setConnectErr := func(err error) {
		sb.mu.Lock()
		sb.connectErr = err
		sb.mu.Unlock()
	}
	waitIdle := func() {
		waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	}

	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	if mgr.list()[0].isFailed() {
		t.Fatal("terminal after a single connect failure")
	}

	setConnectErr(nil)
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	if sb.countCalls("settle") != 1 {
		t.Fatal("successful reconnect never settled a turn")
	}

	// A new failure after a success starts a fresh budget: the session must
	// not go terminal on the first timeout of the new chain.
	setConnectErr(errors.New("timeout"))
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	if mgr.list()[0].isFailed() {
		t.Fatal("failure budget carried over from an earlier connection chain")
	}
}

func TestRemoveSessionRetainsOnFailedRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{releaseErr: errors.New("apiserver down")}
	mgr, _ := newTestManager(ctx, sb, nil)
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")

	mgr.RemoveSession("s1")
	if len(mgr.list()) != 1 {
		t.Fatal("entry dropped despite failed release; the claim would leak")
	}
	// The retained entry must not accept new work.
	if err := mgr.Dispatch(testEvent("s1")); err == nil {
		t.Fatal("stopped session accepted a new event")
	}
	// A retry with a working release drops it.
	sb.mu.Lock()
	sb.releaseErr = nil
	sb.mu.Unlock()
	mgr.RemoveSession("s1")
	waitForCond(t, time.Second, func() bool { return len(mgr.list()) == 0 }, "session not dropped after successful retry")
}

func TestRemoveSessionReleasesSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{}
	mgr, _ := newTestManager(ctx, sb, nil)
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	mgr.RemoveSession("s1")
	waitForCond(t, time.Second, func() bool { return sb.countCalls("release") == 1 }, "release not called")
	if len(mgr.list()) != 0 {
		t.Fatal("session not dropped")
	}
}
