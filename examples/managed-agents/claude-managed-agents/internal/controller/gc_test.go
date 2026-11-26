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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// containsCall reports whether the fake recorded a call with the prefix.
func containsCall(fake *fakeSandbox, prefix string) bool {
	for _, c := range fake.callList() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// seedSession registers a session in the manager, waits until the seed event
// is fully committed (the loop records completion after WaitWorkFinished
// returns), and only then backdates the idle clock. Same-package access to
// sessionState is intentional.
func seedSession(t *testing.T, mgr *SessionManager, sessionID string, fake *fakeSandbox, idleFor time.Duration) {
	t.Helper()
	if err := mgr.Dispatch(WorkEvent{SessionID: sessionID, WorkID: "seed"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForCond(t, "seed event processed", func() bool {
		return containsCall(fake, "wait")
	})
	s := sessionOf(mgr, sessionID)
	// The completion commit (busy=false + lastDone) happens after the fake
	// records "wait"; backdating before it lands would be overwritten.
	waitForCond(t, "session idle", func() bool { return !s.isBusy() })
	s.mu.Lock()
	s.lastDone = time.Now().Add(-idleFor)
	s.mu.Unlock()
}

// countCalls returns how often the fake recorded the exact call name.
func countCalls(fake *fakeSandbox, name string) int {
	n := 0
	for _, c := range fake.callList() {
		if c == name {
			n++
		}
	}
	return n
}

func TestGC_SuspendsIdleSession(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })
	seedSession(t, mgr, "sesn_1", fake, 45*time.Second)

	gc := NewGC(mgr, 30*time.Second, 30*time.Minute, time.Second, logr.Discard())
	gc.Sweep(context.Background())

	// The sweep queues the suspend; the session loop executes it. Wait for
	// the state commit (markSuspended runs after the Suspend call records),
	// then check the call was recorded.
	waitForCond(t, "suspension committed", func() bool {
		return sessionOf(mgr, "sesn_1").suspendedState()
	})
	if !containsCall(fake, "suspend") {
		t.Errorf("calls = %v, want a suspend", fake.callList())
	}
}

func TestGC_SkipsFreshSessions(t *testing.T) {
	fresh := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fresh })
	seedSession(t, mgr, "sesn_fresh", fresh, 5*time.Second) // below threshold

	gc := NewGC(mgr, 30*time.Second, 30*time.Minute, time.Second, logr.Discard())
	gc.Sweep(context.Background())

	time.Sleep(100 * time.Millisecond) // give a wrongful suspend time to run
	if containsCall(fresh, "suspend") {
		t.Errorf("fresh session suspended: %v", fresh.callList())
	}
}

func TestGC_SuspendsOnlyOnce(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })
	seedSession(t, mgr, "sesn_1", fake, 45*time.Second)

	gc := NewGC(mgr, 30*time.Second, 30*time.Minute, time.Second, logr.Discard())
	gc.Sweep(context.Background())
	waitForCond(t, "first suspend", func() bool { return countCalls(fake, "suspend") >= 1 })
	gc.Sweep(context.Background())
	time.Sleep(100 * time.Millisecond)

	if n := countCalls(fake, "suspend"); n != 1 {
		t.Errorf("suspend called %d times, want 1", n)
	}
}

func TestGC_ReleasesAndRemovesAfterReleaseWindow(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })
	seedSession(t, mgr, "sesn_1", fake, 45*time.Minute)

	gc := NewGC(mgr, 30*time.Second, 30*time.Minute, time.Second, logr.Discard())
	gc.Sweep(context.Background())

	if !containsCall(fake, "release") {
		t.Errorf("calls = %v, want a release", fake.callList())
	}
	mgr.mu.Lock()
	_, still := mgr.sessions["sesn_1"]
	mgr.mu.Unlock()
	if still {
		t.Error("released session still tracked by the manager")
	}
}

// TestGC_SuspendWaitsForInFlightWork covers the review finding that a
// suspend decided by the sweep must not terminate a pod with work in
// flight. The request is queued behind the running event, and because the
// finished work refreshes the idle clock, the stale request is cancelled at
// processing time — the next sweep suspends once the session is genuinely
// idle again.
func TestGC_SuspendWaitsForInFlightWork(t *testing.T) {
	wait := make(chan struct{})
	fake := &fakeSandbox{waitBlock: wait}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_1", WorkID: "w1"})
	waitForCond(t, "w1 in flight", func() bool { return len(fake.callList()) == 2 })

	s := sessionOf(mgr, "sesn_1")
	s.mu.Lock()
	s.lastDone = time.Now().Add(-45 * time.Second) // look idle to the sweep
	s.mu.Unlock()

	gc := NewGC(mgr, 30*time.Second, 30*time.Minute, time.Second, logr.Discard())
	gc.Sweep(context.Background())
	time.Sleep(100 * time.Millisecond)
	if containsCall(fake, "suspend") {
		t.Fatal("suspend ran while work was in flight")
	}

	close(wait)
	// The queued request is processed after the work, but the completion
	// refreshed lastDone, so the stale suspend is skipped.
	waitForCond(t, "sentinel processed", func() bool { return !s.isBusy() })
	if containsCall(fake, "suspend") {
		t.Fatal("stale suspend ran right after work finished")
	}

	// Once genuinely idle again, the next sweep suspends.
	s.mu.Lock()
	s.lastDone = time.Now().Add(-45 * time.Second)
	s.mu.Unlock()
	gc.Sweep(context.Background())
	waitForCond(t, "suspend after idle", func() bool { return containsCall(fake, "suspend") })
}
