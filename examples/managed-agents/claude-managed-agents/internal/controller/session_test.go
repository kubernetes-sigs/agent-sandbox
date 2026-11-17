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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// fakeSandbox records the call sequence and can block or fail on demand.
type fakeSandbox struct {
	mu    sync.Mutex
	calls []string

	ensureErr    error         // always fail EnsureReady with this
	ensureErrs   int           // fail EnsureReady this many times, then succeed
	ensureBlock  chan struct{} // if set, EnsureReady waits for it
	inPost       int           // live PostWork calls, for overlap detection
	overlaps     int
	waitBlock    chan struct{}
	waitErr      error             // always fail WaitWorkFinished with this
	postErr      error             // always fail PostWork with this
	postedTokens map[string]string // work ID -> lease token seen by PostWork
}

func (f *fakeSandbox) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeSandbox) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSandbox) EnsureReady(ctx context.Context) error {
	if f.ensureBlock != nil {
		<-f.ensureBlock
	}
	f.mu.Lock()
	err := f.ensureErr
	retries := f.ensureErrs
	if retries > 0 {
		f.ensureErrs--
	}
	f.mu.Unlock()

	if err != nil {
		f.record("ensure:err")
		return err
	}
	if retries > 0 {
		f.record("ensure:err")
		return errors.New("transient ensure failure")
	}
	f.record("ensure")
	return nil
}

func (f *fakeSandbox) PostWork(ctx context.Context, ev WorkEvent) error {
	f.mu.Lock()
	f.inPost++
	if f.inPost > 1 {
		f.overlaps++
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inPost--
		f.mu.Unlock()
	}()
	f.mu.Lock()
	if f.postedTokens == nil {
		f.postedTokens = map[string]string{}
	}
	f.postedTokens[ev.WorkID] = ev.LastHeartbeat
	err := f.postErr
	f.mu.Unlock()
	f.record("post:" + ev.WorkID)
	return err
}

func (f *fakeSandbox) WaitWorkFinished(ctx context.Context) error {
	if f.waitBlock != nil {
		<-f.waitBlock
	}
	f.record("wait")
	f.mu.Lock()
	err := f.waitErr
	f.mu.Unlock()
	return err
}

func (f *fakeSandbox) Suspend(ctx context.Context) error {
	f.record("suspend")
	return nil
}

func (f *fakeSandbox) Release(ctx context.Context) error {
	f.record("release")
	return nil
}

func newTestManager(t *testing.T, factory func(string) sessionSandbox) *SessionManager {
	t.Helper()
	return NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         factory,
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		QueueLen:        8,
	})
}

// waitForCond polls cond until true or fails the test.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSession_FIFOSerialization(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	for i := 1; i <= 3; i++ {
		if err := mgr.Dispatch(WorkEvent{SessionID: "sesn_1", WorkID: fmt.Sprintf("work_%d", i)}); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
	}

	want := []string{
		"ensure", "post:work_1", "wait",
		"ensure", "post:work_2", "wait",
		"ensure", "post:work_3", "wait",
	}
	waitForCond(t, "all events processed", func() bool {
		return len(fake.callList()) == len(want)
	})
	got := fake.callList()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	fake.mu.Lock()
	overlaps := fake.overlaps
	fake.mu.Unlock()
	if overlaps > 0 {
		t.Errorf("%d overlapping PostWork calls; events must serialize", overlaps)
	}
}

func TestSession_ConcurrentSessionsRunInParallel(t *testing.T) {
	blockA := make(chan struct{})
	blockB := make(chan struct{})
	a := &fakeSandbox{ensureBlock: blockA}
	b := &fakeSandbox{ensureBlock: blockB}
	mgr := newTestManager(t, func(id string) sessionSandbox {
		if id == "sesn_a" {
			return a
		}
		return b
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_a", WorkID: "work_a"})
	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_b", WorkID: "work_b"})

	close(blockA)
	close(blockB)

	waitForCond(t, "both works processed", func() bool {
		return len(a.callList()) == 3 && len(b.callList()) == 3
	})
}

func TestSession_RetriesThenSucceeds(t *testing.T) {
	fake := &fakeSandbox{ensureErrs: 2}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_1", WorkID: "work_1"})

	waitForCond(t, "work processed after retries", func() bool {
		calls := fake.callList()
		return len(calls) == 5 && calls[4] == "wait" // err, err, ensure, post, wait
	})
}

func TestSession_AbandonsAfterMaxAttempts(t *testing.T) {
	fake := &fakeSandbox{ensureErr: errors.New("boom")}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_1", WorkID: "work_1"})

	waitForCond(t, "attempts exhausted", func() bool {
		return len(fake.callList()) == 3 // ProcessAttempts = 3
	})

	// The session goroutine survives: fix the fake and dispatch again.
	fake.mu.Lock()
	fake.ensureErr = nil
	fake.mu.Unlock()
	_ = mgr.Dispatch(WorkEvent{SessionID: "sesn_1", WorkID: "work_2"})
	waitForCond(t, "next event processed", func() bool {
		calls := fake.callList()
		return calls[len(calls)-1] == "wait"
	})
}

func TestDispatch_FullQueueReturnsError(t *testing.T) {
	block := make(chan struct{})
	slow := &fakeSandbox{waitBlock: block}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return slow },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		QueueLen:        1,
	})

	if err := mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"}); err != nil {
		t.Fatalf("Dispatch(w1): %v", err)
	}
	// Wait until the session goroutine is actually inside w1's
	// WaitWorkFinished (ensure + post recorded, wait not yet), so the queue
	// slot is free for w2 and only w2. Without this the goroutine may not
	// have consumed w1 yet: w2 would be rejected instead, freeing the slot
	// for w3.
	waitForCond(t, "w1 in flight", func() bool {
		return len(slow.callList()) == 2
	})
	if err := mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2"}); err != nil {
		t.Fatalf("Dispatch(w2): %v", err) // queued (len 1)
	}
	err := mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w3"})
	if err == nil {
		t.Error("Dispatch on a full queue must fail so the lease can lapse and the API can re-deliver")
	}
	close(block)
}

// fakeRenewer records renewal calls and returns canned tokens or errors per
// work ID. If block is non-nil, each call waits on it before returning, so
// tests can hold a renewal in flight across the hand-off.
type fakeRenewer struct {
	mu     sync.Mutex
	calls  []string
	tokens map[string]string
	errs   map[string]error
	block  chan struct{}
}

func (f *fakeRenewer) RenewLease(ctx context.Context, workID, lastHeartbeat string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, workID+":"+lastHeartbeat)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[workID]; ok {
		return "", err
	}
	if tok, ok := f.tokens[workID]; ok {
		return tok, nil
	}
	return "renewed-" + workID, nil
}

func (f *fakeRenewer) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func sessionOf(mgr *SessionManager, id string) *sessionState {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.sessions[id]
}

// TestRenewLoop_UpdatesQueuedTokens verifies that queued events get their
// lease tokens renewed while an earlier event is still in flight.
func TestRenewLoop_UpdatesQueuedTokens(t *testing.T) {
	block := make(chan struct{})
	slow := &fakeSandbox{waitBlock: block}
	renewer := &fakeRenewer{}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return slow },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	waitForCond(t, "w1 in flight", func() bool { return len(slow.callList()) == 2 })
	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2", LastHeartbeat: "tok2"})

	waitForCond(t, "w2 lease renewed with its current token", func() bool {
		for _, c := range renewer.callList() {
			if c == "w2:tok2" {
				return true
			}
		}
		return false
	})
	waitForCond(t, "renewed token stored on the queued event", func() bool {
		s := sessionOf(mgr, "s")
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.pending) == 1 && s.pending[0].ev.LastHeartbeat != "tok2"
	})
	close(block)
}

// TestRenewLoop_DropsLostLease verifies a queued event whose renewal fails
// (lease lost or expired) is dropped instead of running with a dead lease;
// CMA re-delivers it to a later poll.
func TestRenewLoop_DropsLostLease(t *testing.T) {
	block := make(chan struct{})
	slow := &fakeSandbox{waitBlock: block}
	renewer := &fakeRenewer{errs: map[string]error{"w2": errors.New("412")}}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return slow },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 in flight", func() bool { return len(slow.callList()) == 2 })
	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2"})

	waitForCond(t, "w2 dropped from the queue", func() bool {
		s := sessionOf(mgr, "s")
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.pending) == 0
	})
	close(block)
}

// TestRenewLoop_RenewsInFlightDuringSlowReadiness verifies the lease keeps
// being renewed while an event is in flight but not yet posted (slow
// EnsureReady), so sandbox startup cannot outlive the hand-off TTL.
func TestRenewLoop_RenewsInFlightDuringSlowReadiness(t *testing.T) {
	ensure := make(chan struct{})
	fake := &fakeSandbox{ensureBlock: ensure, waitBlock: make(chan struct{})}
	renewer := &fakeRenewer{}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})

	// w1 is popped (in flight) but stuck in EnsureReady; renewal must cover it.
	waitForCond(t, "in-flight lease renewed during EnsureReady", func() bool {
		for _, c := range renewer.callList() {
			if c == "w1:tok1" {
				return true
			}
		}
		return false
	})
	close(ensure)
	close(fake.waitBlock)
}

// TestPostWork_UsesRenewedToken verifies the runner receives the latest
// lease token, not the stale one the event was dispatched with.
func TestPostWork_UsesRenewedToken(t *testing.T) {
	ensure := make(chan struct{})
	fake := &fakeSandbox{ensureBlock: ensure, waitBlock: make(chan struct{})}
	renewer := &fakeRenewer{tokens: map[string]string{"w1": "tok1-renewed"}}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	waitForCond(t, "token renewed while in flight", func() bool {
		s := sessionOf(mgr, "s")
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inFlight != nil && s.inFlight.LastHeartbeat == "tok1-renewed"
	})
	close(ensure)
	waitForCond(t, "work posted", func() bool { return containsCall(fake, "post:w1") })
	close(fake.waitBlock)

	fake.mu.Lock()
	got := fake.postedTokens["w1"]
	fake.mu.Unlock()
	if got != "tok1-renewed" {
		t.Errorf("PostWork token = %q, want the renewed token", got)
	}
}

// TestTakeForPost_WaitsForInFlightRenewal covers the hand-off race: a
// renewal is in flight when EnsureReady finishes, so takeForPost must wait
// for it to commit and PostWork must send the renewed token, not the stale
// one the renewal is rotating.
func TestTakeForPost_WaitsForInFlightRenewal(t *testing.T) {
	ensure := make(chan struct{})
	fake := &fakeSandbox{ensureBlock: ensure, waitBlock: make(chan struct{})}
	renewer := &fakeRenewer{
		tokens: map[string]string{"w1": "tok1-R"},
		block:  make(chan struct{}),
	}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	waitForCond(t, "renewal in flight (blocked)", func() bool { return len(renewer.callList()) > 0 })

	// EnsureReady returns while the renewal is still blocked; takeForPost
	// must not hand the runner the stale token.
	close(ensure)
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "post:w1") {
		t.Fatal("PostWork ran while a renewal was in flight")
	}

	close(renewer.block)
	waitForCond(t, "work posted", func() bool { return containsCall(fake, "post:w1") })
	fake.mu.Lock()
	got := fake.postedTokens["w1"]
	fake.mu.Unlock()
	if got != "tok1-R" {
		t.Errorf("PostWork token = %q, want the renewed token tok1-R", got)
	}
	close(fake.waitBlock)
}

// TestQueuedRenewal_FollowsEventIntoFlight covers the transition race: a
// renewal starts while the event is still queued, the event is popped and
// handed off while that renewal is in flight. takeForPost must wait for the
// renewal to commit, and the renewed token must land on the in-flight event
// even though it left pending mid-renewal.
func TestQueuedRenewal_FollowsEventIntoFlight(t *testing.T) {
	wait := make(chan struct{})
	fake := &fakeSandbox{waitBlock: wait}
	renewer := &fakeRenewer{
		tokens: map[string]string{"w2": "tok2-R"},
		block:  make(chan struct{}),
	}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	waitForCond(t, "w1 in flight", func() bool { return len(fake.callList()) == 2 })
	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2", LastHeartbeat: "tok2"})

	s := sessionOf(mgr, "s")
	waitForCond(t, "w2 renewal in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.renewingID == "w2"
	})

	// w1 finishes; the loop pops w2 and reaches takeForPost while the w2
	// renewal is still blocked.
	close(wait)
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "post:w2") {
		t.Fatal("PostWork ran while a renewal of the same work ID was in flight")
	}

	close(renewer.block)
	waitForCond(t, "w2 posted", func() bool { return containsCall(fake, "post:w2") })
	fake.mu.Lock()
	got := fake.postedTokens["w2"]
	fake.mu.Unlock()
	if got != "tok2-R" {
		t.Errorf("PostWork token = %q, want the renewed token tok2-R", got)
	}
}

// TestRenewClaim_SkipsItemHandedOffAfterSnapshot covers the gap between the
// renew loop's snapshot and its per-item claim: while an earlier renewal is
// blocked, the in-flight item is handed to the runner. The loop must
// recheck eligibility at claim time and never renew the posted item, which
// would rotate the token out from under the runner's first heartbeat.
func TestRenewClaim_SkipsItemHandedOffAfterSnapshot(t *testing.T) {
	ensure := make(chan struct{})
	fake := &fakeSandbox{ensureBlock: ensure}
	renewer := &fakeRenewer{block: make(chan struct{})}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	s := sessionOf(mgr, "s")
	waitForCond(t, "w1 in flight (blocked in EnsureReady)", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inFlight != nil
	})
	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2", LastHeartbeat: "tok2"})

	// A tick snapshots [w2 (queued), w1 (in flight)] and blocks renewing w2.
	waitForCond(t, "w2 renewal in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.renewingID == "w2"
	})

	// Hand w1 to the runner while the w2 renewal is still blocked. w1's
	// claim comes after w2 in the same snapshot.
	close(ensure)
	waitForCond(t, "w1 posted", func() bool { return containsCall(fake, "post:w1") })

	close(renewer.block)
	time.Sleep(50 * time.Millisecond) // let the loop reach w1's claim

	for _, c := range renewer.callList() {
		if strings.HasPrefix(c, "w1:") {
			t.Fatalf("renewal started for w1 after hand-off: %q (calls: %v)", c, renewer.callList())
		}
	}
}

// TestInFlightLeaseLost_SkipsPostWork verifies an in-flight event whose
// lease cannot be renewed is abandoned before any work reaches the runner.
func TestInFlightLeaseLost_SkipsPostWork(t *testing.T) {
	ensure := make(chan struct{})
	fake := &fakeSandbox{ensureBlock: ensure}
	renewer := &fakeRenewer{errs: map[string]error{"w1": errors.New("lease gone")}}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:         func(string) sessionSandbox { return fake },
		Logger:          logr.Discard(),
		RetryBase:       time.Millisecond,
		ProcessAttempts: 3,
		Renewer:         renewer,
		RenewInterval:   5 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1", LastHeartbeat: "tok1"})
	s := sessionOf(mgr, "s")
	// The renew loop records the call before it commits leaseLost; wait for
	// the state itself so releasing readiness cannot race the post check.
	waitForCond(t, "lease marked lost", func() bool { return s.leaseLostForInFlight() })
	close(ensure)

	waitForCond(t, "session back to idle without posting work", func() bool {
		return !s.isBusy() && !containsCall(fake, "post:")
	})
}

// TestRunnerActiveWindow_RefreshedOnAbandonment covers a turn that runs
// longer than the window: the window is measured from when the controller
// stops observing (abandonment), not from when the work was posted.
func TestRunnerActiveWindow_RefreshedOnAbandonment(t *testing.T) {
	wait := make(chan struct{})
	fake := &fakeSandbox{waitBlock: wait, waitErr: errors.New("poll failed")}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:            func(string) sessionSandbox { return fake },
		Logger:             logr.Discard(),
		RetryBase:          time.Millisecond,
		ProcessAttempts:    1,
		RenewInterval:      time.Hour,
		RunnerActiveWindow: 100 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "work posted", func() bool { return containsCall(fake, "post:w1") })

	// The turn outlives the window opened at post time; observation only
	// fails afterwards.
	time.Sleep(150 * time.Millisecond)
	close(wait)

	s := sessionOf(mgr, "s")
	waitForCond(t, "event abandoned", func() bool { return !s.isBusy() })
	// The abandonment refreshed the window, so cleanup is still blocked.
	if s.tryReserveRelease(time.Now(), 0) {
		t.Error("release reserved after abandonment despite the refreshed runner-active window")
	}
	s.requestSuspend(0)
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "suspend") {
		t.Error("suspend ran inside the refreshed runner-active window")
	}
}

// TestUncertainPost_OpensRunnerActiveWindow covers a lost POST response: the
// runner may have started the work, so an uncertain delivery failure must
// open the window even though the controller never saw acceptance.
func TestUncertainPost_OpensRunnerActiveWindow(t *testing.T) {
	fake := &fakeSandbox{postErr: newPostError(errors.New("connection reset"), true)}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:            func(string) sessionSandbox { return fake },
		Logger:             logr.Discard(),
		RetryBase:          time.Millisecond,
		ProcessAttempts:    1,
		RenewInterval:      time.Hour,
		RunnerActiveWindow: time.Hour,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	s := sessionOf(mgr, "s")
	waitForCond(t, "event abandoned", func() bool {
		return containsCall(fake, "post:w1") && !s.isBusy()
	})

	if !s.runnerMayBeActive() {
		t.Error("uncertain delivery did not open the runner-active window")
	}
	if s.tryReserveRelease(time.Now(), 0) {
		t.Error("release reserved while an uncertain delivery may be running")
	}
	s.requestSuspend(0)
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "suspend") {
		t.Error("suspend ran while an uncertain delivery may be running")
	}
}

// TestConfirmedRejectedPost_NoWindow verifies a definite rejection (e.g. 409
// busy) does not open the window: the runner is known not to run this item.
func TestConfirmedRejectedPost_NoWindow(t *testing.T) {
	fake := &fakeSandbox{postErr: newPostError(errors.New("409 busy"), false)}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:            func(string) sessionSandbox { return fake },
		Logger:             logr.Discard(),
		RetryBase:          time.Millisecond,
		ProcessAttempts:    1,
		RenewInterval:      time.Hour,
		RunnerActiveWindow: time.Hour,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	s := sessionOf(mgr, "s")
	waitForCond(t, "event abandoned", func() bool {
		return containsCall(fake, "post:w1") && !s.isBusy()
	})

	if s.runnerMayBeActive() {
		t.Error("confirmed rejection must not open the runner-active window")
	}
	if !s.tryReserveRelease(time.Now(), 0) {
		t.Error("release blocked despite a confirmed rejection")
	}
}

// TestSuspend_SerializedBehindInFlightWork verifies a suspend request is
// processed by the session loop itself, so it can never terminate a pod
// with work in flight.
func TestSuspend_SerializedBehindInFlightWork(t *testing.T) {
	wait := make(chan struct{})
	fake := &fakeSandbox{waitBlock: wait}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 in flight", func() bool { return len(fake.callList()) == 2 })

	s := sessionOf(mgr, "s")
	s.requestSuspend(0) // idle requirement 0: suspend as soon as work ends
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "suspend") {
		t.Fatal("suspend ran while work was in flight")
	}

	close(wait)
	waitForCond(t, "suspend ran after work", func() bool { return containsCall(fake, "suspend") })
	calls := fake.callList()
	want := []string{"ensure", "post:w1", "wait", "suspend"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (%v)", i, calls[i], want[i], calls)
		}
	}
}

// TestSuspend_SkippedWhenWorkArrived verifies the sentinel rechecks idleness
// when it is processed: work that arrived after the GC decision cancels the
// suspension instead of suspending a freshly active sandbox.
func TestSuspend_SkippedWhenWorkArrived(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 done", func() bool { return containsCall(fake, "wait") })
	s := sessionOf(mgr, "s")
	waitForCond(t, "session idle", func() bool { return !s.isBusy() })

	// Sentinel demands 1h of idleness; the session only has moments.
	s.requestSuspend(time.Hour)
	waitForCond(t, "sentinel processed", func() bool { return !s.isBusy() })
	if containsCall(fake, "suspend") {
		t.Error("suspend ran despite insufficient idle time at processing")
	}
}

// TestReleaseReservation_RechecksIdle verifies the reservation re-validates
// the idle threshold atomically: work that finished between the GC's check
// and the reservation keeps the session alive.
func TestReleaseReservation_RechecksIdle(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 done", func() bool { return containsCall(fake, "wait") })
	s := sessionOf(mgr, "s")
	waitForCond(t, "session idle", func() bool { return !s.isBusy() })

	// lastDone is fresh; a stale GC decision (based on an old backdated
	// read) must not reserve the session.
	if s.tryReserveRelease(time.Now().Add(-45*time.Minute), 30*time.Minute) {
		t.Error("tryReserveRelease reserved a session whose lastDone is fresh")
	}
	if s.tryReserveRelease(time.Now(), 30*time.Minute) == false {
		// idle is ~0 < 30m: also must refuse.
	} else {
		t.Error("tryReserveRelease reserved an idle-for-nothing session")
	}
}

// TestRemove_StopsSessionLoops verifies removing a session terminates its
// goroutines instead of leaving them blocked until manager shutdown.
func TestRemove_StopsSessionLoops(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 done", func() bool { return containsCall(fake, "wait") })
	s := sessionOf(mgr, "s")

	mgr.remove("s")
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session run loop did not exit after remove")
	}
}

// TestRunnerActiveWindow_BlocksSuspendAndRelease covers the case where the
// controller gave up waiting for a work item the runner accepted: the turn
// may still be executing in the sandbox, so suspend and release must stay
// off until the bounded runner-active window passes.
func TestRunnerActiveWindow_BlocksSuspendAndRelease(t *testing.T) {
	fake := &fakeSandbox{waitErr: errors.New("status polls failed")}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:            func(string) sessionSandbox { return fake },
		Logger:             logr.Discard(),
		RetryBase:          time.Millisecond,
		ProcessAttempts:    2,
		RenewInterval:      time.Hour,
		RunnerActiveWindow: 150 * time.Millisecond,
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	// The event is abandoned after both attempts fail in WaitWorkFinished,
	// but the runner accepted the work, so the window is now active.
	waitForCond(t, "event abandoned, session idle", func() bool {
		s := sessionOf(mgr, "s")
		return s != nil && !s.isBusy() && containsCall(fake, "wait")
	})
	s := sessionOf(mgr, "s")

	s.requestSuspend(0)
	time.Sleep(50 * time.Millisecond)
	if containsCall(fake, "suspend") {
		t.Error("suspend ran inside the runner-active window")
	}
	if s.tryReserveRelease(time.Now(), 0) {
		t.Error("release reserved inside the runner-active window")
	}

	// After the window passes, both proceed.
	time.Sleep(150 * time.Millisecond)
	s.requestSuspend(0)
	waitForCond(t, "suspend after window", func() bool { return containsCall(fake, "suspend") })
}

// TestRunnerActiveWindow_ClearedOnConfirmedCompletion verifies a normally
// finished turn does not keep the window active.
func TestRunnerActiveWindow_ClearedOnConfirmedCompletion(t *testing.T) {
	fake := &fakeSandbox{}
	mgr := NewSessionManager(t.Context(), SessionManagerConfig{
		Factory:            func(string) sessionSandbox { return fake },
		Logger:             logr.Discard(),
		RetryBase:          time.Millisecond,
		ProcessAttempts:    2,
		RenewInterval:      time.Hour,
		RunnerActiveWindow: time.Hour, // long window; must be cleared explicitly
	})

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 done", func() bool { return containsCall(fake, "wait") })
	s := sessionOf(mgr, "s")
	waitForCond(t, "session idle", func() bool { return !s.isBusy() })

	s.requestSuspend(0)
	waitForCond(t, "suspend runs after confirmed completion", func() bool {
		return containsCall(fake, "suspend")
	})
}

// TestDispatch_RejectedWhileReleaseReserved verifies the GC release
// reservation excludes new work: a reserved session refuses events (their
// leases lapse for re-delivery), and a removed session is recreated fresh.
func TestDispatch_RejectedWhileReleaseReserved(t *testing.T) {
	var created int
	fake := &fakeSandbox{}
	mgr := newTestManager(t, func(string) sessionSandbox { created++; return fake })

	_ = mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w1"})
	waitForCond(t, "w1 processed", func() bool { return containsCall(fake, "wait") })
	s := sessionOf(mgr, "s")
	waitForCond(t, "session idle", func() bool { return !s.isBusy() })

	s.mu.Lock()
	s.lastDone = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if !s.tryReserveRelease(time.Now(), 30*time.Minute) {
		t.Fatal("tryReserveRelease = false on an idle session past the release threshold")
	}
	if err := mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w2"}); err == nil {
		t.Error("Dispatch to a release-reserved session must fail")
	}

	mgr.remove("s")
	if err := mgr.Dispatch(WorkEvent{SessionID: "s", WorkID: "w3"}); err != nil {
		t.Fatalf("Dispatch after remove: %v", err)
	}
	waitForCond(t, "fresh session created", func() bool { return created == 2 })
}
