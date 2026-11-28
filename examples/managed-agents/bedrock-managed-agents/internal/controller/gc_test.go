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
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// gcFixture is a manager with one idle session whose sandbox is a fake,
// plus the GC under test.
type gcFixture struct {
	mgr *SessionManager
	sb  *fakeSandbox
	gc  *GC
	s   *sessionState
}

func newGCFixture(t *testing.T, ctx context.Context, api AgentAPI, idleFor time.Duration) *gcFixture {
	t.Helper()
	sb := &fakeSandbox{}
	mgr := NewSessionManager(ctx, SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return sb },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 1,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return !mgr.list()[0].isBusy() }, "session stayed busy")
	s := mgr.list()[0]
	s.mu.Lock()
	s.lastDone = time.Now().Add(-idleFor)
	s.mu.Unlock()
	gc := NewGC(mgr, api, time.Minute, 10*time.Minute, time.Hour, logr.Discard())
	return &gcFixture{mgr: mgr, sb: sb, gc: gc, s: s}
}

func inactiveAPI() *FakeAPI {
	return &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, Status: "idle"},
	}}
}

func TestSweepSuspendsIdleSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newGCFixture(t, ctx, inactiveAPI(), 2*time.Minute)
	f.gc.Sweep(ctx)
	waitForCond(t, time.Second, func() bool { return f.s.suspendedState() }, "suspend never ran")
	if n := f.sb.countCalls("suspend"); n != 1 {
		t.Fatalf("suspend calls = %d, want exactly 1", n)
	}
}

func TestSweepReleasesPastThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newGCFixture(t, ctx, inactiveAPI(), 20*time.Minute)
	f.gc.Sweep(ctx)
	waitForCond(t, time.Second, func() bool { return f.sb.countCalls("release") == 1 }, "release never ran")
	// The entry stays tracked as released so idle re-deliveries cannot
	// claim a replacement; further sweeps must not release again.
	waitForCond(t, time.Second, func() bool { return f.s.isReleased() }, "session not marked released")
	f.gc.Sweep(ctx)
	if n := f.sb.countCalls("release"); n != 1 {
		t.Fatalf("release calls = %d, want exactly 1", n)
	}
}

func TestSweepVetoedByActiveTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, ActiveTurn: true, Status: "in_progress"},
	}}
	f := newGCFixture(t, ctx, api, 20*time.Minute)
	f.gc.Sweep(ctx)
	time.Sleep(30 * time.Millisecond)
	if f.sb.countCalls("suspend")+f.sb.countCalls("release") != 0 {
		t.Fatal("lifecycle action despite active turn")
	}
}

func TestSweepSkipsOnAPIError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &FakeAPI{GetErr: errors.New("500"), Sessions: map[string]SessionInfo{}}
	f := newGCFixture(t, ctx, api, 20*time.Minute)
	f.gc.Sweep(ctx)
	time.Sleep(30 * time.Millisecond)
	if f.sb.countCalls("suspend")+f.sb.countCalls("release") != 0 {
		t.Fatal("lifecycle action despite unknown platform state (must be conservative)")
	}
}

func TestSweepReleasesOnSessionNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &FakeAPI{Sessions: map[string]SessionInfo{}} // s1 deleted
	// Idle past suspend but NOT past release: a platform-deleted session
	// releases immediately because the platform will never reclaim our
	// compute for it.
	f := newGCFixture(t, ctx, api, 2*time.Minute)
	f.gc.Sweep(ctx)
	waitForCond(t, time.Second, func() bool { return f.sb.countCalls("release") == 1 }, "release never ran")
	waitForCond(t, time.Second, func() bool { return f.s.isReleased() }, "session not marked released")
	// The reconciler drops the entry entirely once the platform 404s.
	f.mgr.RemoveSession("s1")
	waitForCond(t, time.Second, func() bool { return len(f.mgr.list()) == 0 }, "session not dropped")
}

func TestSweepSkipsRecentlyActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := inactiveAPI()
	f := newGCFixture(t, ctx, api, 10*time.Second) // below the 1m threshold
	f.gc.Sweep(ctx)
	time.Sleep(30 * time.Millisecond)
	if f.sb.countCalls("suspend") != 0 {
		t.Fatal("suspended a recently active session")
	}
	if api.GetCalls != 0 {
		t.Fatalf("platform queried for a session below the idle threshold: %d calls", api.GetCalls)
	}
}

func TestSweepSkipsBusySession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &fakeSandbox{startBlock: make(chan struct{})}
	defer close(sb.startBlock)
	mgr := NewSessionManager(ctx, SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return sb },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 1,
	})
	if err := mgr.Dispatch(testEvent("s1")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, time.Second, func() bool { return sb.countCalls("start:1") == 1 }, "event not started")
	s := mgr.list()[0]
	s.mu.Lock()
	s.lastDone = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	gc := NewGC(mgr, inactiveAPI(), time.Minute, 10*time.Minute, time.Hour, logr.Discard())
	gc.Sweep(ctx)
	time.Sleep(30 * time.Millisecond)
	if sb.countCalls("suspend")+sb.countCalls("release") != 0 {
		t.Fatal("lifecycle action on a busy session")
	}
}

func TestSweepReleaseBlockedByRunnerWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newGCFixture(t, ctx, inactiveAPI(), 20*time.Minute)
	// An unconfirmed turn keeps release off even past the threshold.
	f.s.refreshRunnerActiveWindow()
	f.gc.Sweep(ctx)
	time.Sleep(30 * time.Millisecond)
	if f.sb.countCalls("release") != 0 {
		t.Fatal("released inside the runner-active window")
	}
}
