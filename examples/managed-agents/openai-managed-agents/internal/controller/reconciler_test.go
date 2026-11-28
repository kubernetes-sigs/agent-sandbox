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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// recDispatcher records what the reconciler handed to the session manager.
type recDispatcher struct {
	mu        sync.Mutex
	events    []SessionEvent
	tracked   []string
	removed   []string
	dispatchN int
	// dispatchErr, when set, simulates "session already active" dedup.
	dispatchErr error
}

func (d *recDispatcher) Dispatch(ev SessionEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dispatchN++
	if d.dispatchErr != nil {
		return d.dispatchErr
	}
	d.events = append(d.events, ev)
	return nil
}

func (d *recDispatcher) Track(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tracked = append(d.tracked, id)
}

func (d *recDispatcher) RemoveSession(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, id)
}

func (d *recDispatcher) snapshot() ([]SessionEvent, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]SessionEvent(nil), d.events...), append([]string(nil), d.removed...)
}

func (d *recDispatcher) trackedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.tracked...)
}

func TestPollDispatchesOnlyConfirmedConnections(t *testing.T) {
	api := &FakeAPI{
		// Stale list: s3 still shows NeedsConnection, s1 resolved.
		ListOverride: []SessionInfo{
			{ID: "s1", EnvironmentID: "e1", RemoteURL: "wss://1", NeedsConnection: true, Managed: true},
			{ID: "s3", EnvironmentID: "e3", RemoteURL: "wss://3", NeedsConnection: true, Managed: true},
			{ID: "s4", EnvironmentID: "e4", RemoteURL: "wss://4", NeedsConnection: true, Managed: true},
		},
		Sessions: map[string]SessionInfo{
			"s1": {ID: "s1", EnvironmentID: "e1", RemoteURL: "wss://1", NeedsConnection: true, Managed: true},
			"s3": {ID: "s3", NeedsConnection: false, ActiveTurn: true, Managed: true}, // resolved since list
			"s4": {ID: "s4", EnvironmentID: "e4", RemoteURL: "wss://4", NeedsConnection: true, Managed: true},
		},
	}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := d.snapshot()
	if len(events) != 2 {
		t.Fatalf("dispatched %+v, want s1 and s4 only", events)
	}
	seen := map[string]bool{events[0].SessionID: true, events[1].SessionID: true}
	if !seen["s1"] || !seen["s4"] || seen["s3"] {
		t.Fatalf("wrong set dispatched: %+v", events)
	}
	if events[0].RemoteURL == "" {
		t.Fatal("event must carry the remote URL for the runner")
	}
}

func TestPollIgnoresUnmanagedAndNoAction(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: false, NeedsConnection: true}, // not ours
		"s2": {ID: "s2", Managed: true, NeedsConnection: false, ActiveTurn: true},
	}}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := d.snapshot()
	if len(events) != 0 {
		t.Fatalf("dispatched %+v, want none", events)
	}
}

func TestPollTracksConnectedManagedSessions(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		// Connected and idle: no event may be dispatched, but the session
		// must be tracked so GC can clean up a sandbox it retained across a
		// controller restart.
		"s2": {ID: "s2", Managed: true, NeedsConnection: false, ActiveTurn: false},
	}}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := d.snapshot()
	if len(events) != 0 {
		t.Fatalf("dispatched %+v, want none", events)
	}
	if tracked := d.trackedIDs(); len(tracked) != 1 || tracked[0] != "s2" {
		t.Fatalf("tracked = %v, want [s2]", tracked)
	}
}

func TestPollDedupesAgainstActiveSessions(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", EnvironmentID: "e1", RemoteURL: "wss://1", NeedsConnection: true, Managed: true},
	}}
	d := &recDispatcher{dispatchErr: ErrAlreadyActive}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	// An already-active session is not an error: PollOnce must succeed.
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatalf("dedup should not fail poll: %v", err)
	}
	events, _ := d.snapshot()
	if len(events) != 0 {
		t.Fatalf("dup event recorded: %+v", events)
	}
}

func TestPollSessionGoneTriggersRemove(t *testing.T) {
	api := &FakeAPI{
		ListOverride: []SessionInfo{
			{ID: "s9", NeedsConnection: true, Managed: true},
		},
		Sessions: map[string]SessionInfo{}, // s9 deleted since list
	}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, removed := d.snapshot()
	if len(events) != 0 {
		t.Fatalf("dispatched %+v, want none", events)
	}
	if len(removed) != 1 || removed[0] != "s9" {
		t.Fatalf("removed = %v, want [s9]", removed)
	}
}

func TestPollGetErrorSkipsSession(t *testing.T) {
	api := &FakeAPI{
		ListOverride: []SessionInfo{{ID: "s1", NeedsConnection: true, Managed: true}},
		GetErr:       errors.New("500"),
	}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	// A transient Get failure must not fail the whole poll or dispatch.
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll should tolerate per-session get errors: %v", err)
	}
	events, removed := d.snapshot()
	if len(events) != 0 || len(removed) != 0 {
		t.Fatalf("events=%+v removed=%v, want none", events, removed)
	}
}

func TestPollListErrorPropagates(t *testing.T) {
	api := &FakeAPI{ListErr: errors.New("429"), Sessions: map[string]SessionInfo{}}
	d := &recDispatcher{}
	r := NewReconciler(api, d, time.Second, logr.Discard())
	if err := r.PollOnce(context.Background()); err == nil {
		t.Fatal("want list error propagated for backoff")
	}
}

func TestRunRetriesWithBackoff(t *testing.T) {
	api := &FakeAPI{ListErr: errors.New("429"), Sessions: map[string]SessionInfo{}}
	d := &recDispatcher{}
	r := NewReconciler(api, d, 10*time.Millisecond, logr.Discard())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx) // returns on ctx done
	api.mu.Lock()
	calls := api.ListCalls
	api.mu.Unlock()
	// Backoff must keep the call count bounded (not a hot loop): with a 10ms
	// base and 300ms window, a hot loop would do ~30 calls; backoff caps well below.
	if calls < 2 {
		t.Fatalf("expected retries, got %d calls", calls)
	}
	if calls > 12 {
		t.Fatalf("no backoff: %d calls in 300ms", calls)
	}
}
