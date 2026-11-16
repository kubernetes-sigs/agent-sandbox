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

package controllers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

const testDeferralWindow = 500 * time.Millisecond

var testDeferralKey = types.NamespacedName{Namespace: "default", Name: "sandbox-a"}

// newTestDeferredWriteClock returns a deferredWriteClock driven by a fake
// clock, plus a function that moves that clock forward.
func newTestDeferredWriteClock() (*deferredWriteClock, func(time.Duration)) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &deferredWriteClock{now: func() time.Time { return now }}
	return clock, func(d time.Duration) { now = now.Add(d) }
}

// newTestWriteDeferral returns a fresh per-pass view over clock, like the one
// Reconcile builds at the start of every pass.
func newTestWriteDeferral(clock *deferredWriteClock) *writeDeferral {
	return &writeDeferral{clock: clock, key: testDeferralKey, window: testDeferralWindow}
}

func TestDeferredWriteClockObserveFirstObservation(t *testing.T) {
	clock, _ := newTestDeferredWriteClock()

	due, wait := clock.observe(testDeferralKey, testDeferralWindow)

	require.False(t, due)
	require.Equal(t, testDeferralWindow, wait)
}

func TestDeferredWriteClockObserveAfterElapsed(t *testing.T) {
	tests := []struct {
		name     string
		elapsed  time.Duration
		wantDue  bool
		wantWait time.Duration
	}{
		{
			name:     "immediately after the first observation",
			elapsed:  0,
			wantDue:  false,
			wantWait: testDeferralWindow,
		},
		{
			name:     "partway through the window",
			elapsed:  100 * time.Millisecond,
			wantDue:  false,
			wantWait: 400 * time.Millisecond,
		},
		{
			name:     "one nanosecond before the deadline",
			elapsed:  testDeferralWindow - time.Nanosecond,
			wantDue:  false,
			wantWait: time.Nanosecond,
		},
		{
			name:     "exactly at the deadline",
			elapsed:  testDeferralWindow,
			wantDue:  true,
			wantWait: 0,
		},
		{
			name:     "one nanosecond after the deadline",
			elapsed:  testDeferralWindow + time.Nanosecond,
			wantDue:  true,
			wantWait: 0,
		},
		{
			name:     "long after the deadline",
			elapsed:  10 * testDeferralWindow,
			wantDue:  true,
			wantWait: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock, advance := newTestDeferredWriteClock()
			clock.observe(testDeferralKey, testDeferralWindow) // arms the window
			advance(tc.elapsed)

			due, wait := clock.observe(testDeferralKey, testDeferralWindow)

			require.Equal(t, tc.wantDue, due)
			require.Equal(t, tc.wantWait, wait)
		})
	}
}

// Re-observing a pending key must not restart its window. If it did, a request
// redelivered more often than the window would never get its write flushed.
func TestDeferredWriteClockObserveDoesNotRearmWindow(t *testing.T) {
	clock, advance := newTestDeferredWriteClock()
	clock.observe(testDeferralKey, testDeferralWindow)

	advance(300 * time.Millisecond)
	due, wait := clock.observe(testDeferralKey, testDeferralWindow)
	require.False(t, due)
	require.Equal(t, 200*time.Millisecond, wait)

	advance(200 * time.Millisecond)
	due, wait = clock.observe(testDeferralKey, testDeferralWindow)
	require.True(t, due)
	require.Zero(t, wait)
}

func TestDeferredWriteClockObserveKeysAreIndependent(t *testing.T) {
	otherKey := types.NamespacedName{Namespace: "default", Name: "sandbox-b"}
	clock, advance := newTestDeferredWriteClock()
	clock.observe(testDeferralKey, testDeferralWindow)

	advance(300 * time.Millisecond)
	// A second key starts its own window rather than inheriting the first one's.
	due, wait := clock.observe(otherKey, testDeferralWindow)
	require.False(t, due)
	require.Equal(t, testDeferralWindow, wait)

	advance(200 * time.Millisecond) // first key is at 500ms, second key at 200ms
	due, wait = clock.observe(testDeferralKey, testDeferralWindow)
	require.True(t, due)
	require.Zero(t, wait)

	due, wait = clock.observe(otherKey, testDeferralWindow)
	require.False(t, due)
	require.Equal(t, 300*time.Millisecond, wait)
}

func TestDeferredWriteClockZeroValue(t *testing.T) {
	// The zero value has a nil map and a nil now func, which falls back to
	// time.Now. A large window keeps the assertions independent of wall time.
	var clock deferredWriteClock
	const window = time.Hour

	due, wait := clock.observe(testDeferralKey, window)
	require.False(t, due)
	require.Equal(t, window, wait)

	due, wait = clock.observe(testDeferralKey, window)
	require.False(t, due)
	require.Positive(t, wait)
	require.LessOrEqual(t, wait, window)
}

func TestDeferredWriteClockClear(t *testing.T) {
	clock, advance := newTestDeferredWriteClock()
	clock.observe(testDeferralKey, testDeferralWindow)
	advance(testDeferralWindow)

	clock.clear(testDeferralKey)

	// The old deadline is forgotten: the key starts a fresh window instead of
	// being reported as due.
	due, wait := clock.observe(testDeferralKey, testDeferralWindow)
	require.False(t, due)
	require.Equal(t, testDeferralWindow, wait)
}

func TestDeferredWriteClockClearOnlyAffectsGivenKey(t *testing.T) {
	otherKey := types.NamespacedName{Namespace: "default", Name: "sandbox-b"}
	clock, advance := newTestDeferredWriteClock()
	clock.observe(testDeferralKey, testDeferralWindow)
	clock.observe(otherKey, testDeferralWindow)
	advance(testDeferralWindow)

	clock.clear(testDeferralKey)

	due, _ := clock.observe(otherKey, testDeferralWindow)
	require.True(t, due)
}

func TestDeferredWriteClockClearUnknownKey(t *testing.T) {
	var clock deferredWriteClock // zero value: the map has never been allocated

	require.NotPanics(t, func() { clock.clear(testDeferralKey) })
}

func TestWriteDeferralShouldWriteDefersWhenNotDue(t *testing.T) {
	clock, _ := newTestDeferredWriteClock()
	wd := newTestWriteDeferral(clock)

	require.False(t, wd.shouldWrite())
	require.True(t, wd.deferred)
	require.Equal(t, testDeferralWindow, wd.wait)
}

func TestWriteDeferralShouldWriteWritesWhenDue(t *testing.T) {
	clock, advance := newTestDeferredWriteClock()
	// The first pass arms the window and defers its write.
	require.False(t, newTestWriteDeferral(clock).shouldWrite())
	advance(testDeferralWindow)

	// The requeued pass is a new per-pass view over the same clock: it writes
	// and has nothing left to requeue for.
	wd := newTestWriteDeferral(clock)
	require.True(t, wd.shouldWrite())
	require.False(t, wd.deferred)
	require.Zero(t, wd.wait)
}

// A write site that is due must not undo a deferral recorded earlier in the
// same pass, so Reconcile still requeues for the site that skipped its write.
func TestWriteDeferralShouldWriteDueKeepsEarlierDeferral(t *testing.T) {
	clock, advance := newTestDeferredWriteClock()
	wd := newTestWriteDeferral(clock)
	require.False(t, wd.shouldWrite())
	advance(testDeferralWindow)

	require.True(t, wd.shouldWrite())
	require.True(t, wd.deferred)
	require.Equal(t, testDeferralWindow, wd.wait)
}

func TestWriteDeferralShouldWriteKeepsShortestWait(t *testing.T) {
	clock, advance := newTestDeferredWriteClock()
	wd := newTestWriteDeferral(clock)

	require.False(t, wd.shouldWrite())
	require.Equal(t, testDeferralWindow, wd.wait)

	advance(100 * time.Millisecond)
	require.False(t, wd.shouldWrite())
	require.Equal(t, 400*time.Millisecond, wd.wait)

	// A site reporting a longer remaining window must not stretch the requeue.
	wd.wait = 50 * time.Millisecond
	require.False(t, wd.shouldWrite())
	require.Equal(t, 50*time.Millisecond, wd.wait)
}
