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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func expKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "default", Name: name}
}

func TestWarmPoolExpectations_CreateLifecycle(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	// No expectations recorded yet: satisfied.
	require.True(t, e.SatisfiedExpectations(key))

	// Record 2 creations.
	require.True(t, e.TryExpectCreations(key, 2))
	require.False(t, e.SatisfiedExpectations(key), "unobserved creations must block")

	// A second raise while the first is outstanding must be refused; this is
	// the gate that prevents rapid re-reconciles from re-creating toward the
	// target off a stale cache (#1215).
	require.False(t, e.TryExpectCreations(key, 2))

	// Observing both creations satisfies the pool again.
	e.CreationObserved(key)
	require.False(t, e.SatisfiedExpectations(key))
	e.CreationObserved(key)
	require.True(t, e.SatisfiedExpectations(key))

	// And a new raise is allowed again.
	require.True(t, e.TryExpectCreations(key, 1))
	e.CreationObserved(key)
	require.True(t, e.SatisfiedExpectations(key))
}

func TestWarmPoolExpectations_TryExpectRejectsNonPositive(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")
	require.False(t, e.TryExpectCreations(key, 0))
	require.False(t, e.TryExpectCreations(key, -3))
	require.True(t, e.SatisfiedExpectations(key), "rejected raises must not record state")
}

func TestWarmPoolExpectations_LowerCreationsForFailedCreates(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	require.True(t, e.TryExpectCreations(key, 3))
	// Two creates failed: no watch event will ever arrive for them.
	e.LowerCreations(key, 2)
	require.False(t, e.SatisfiedExpectations(key))
	e.CreationObserved(key)
	require.True(t, e.SatisfiedExpectations(key))

	// Lowering below zero clamps and never underflows.
	require.True(t, e.TryExpectCreations(key, 1))
	e.LowerCreations(key, 5)
	require.True(t, e.SatisfiedExpectations(key))
}

func TestWarmPoolExpectations_DeleteLifecycle(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")
	uid1, uid2 := types.UID("uid-1"), types.UID("uid-2")

	e.ExpectDeletion(key, uid1)
	e.ExpectDeletion(key, uid2)
	require.False(t, e.SatisfiedExpectations(key), "unobserved deletions must block")
	require.True(t, e.IsPendingDeletion(key, uid1))
	require.True(t, e.IsPendingDeletion(key, uid2))
	require.False(t, e.IsPendingDeletion(key, types.UID("uid-other")))

	// Creations cannot be raised while deletions are outstanding.
	require.False(t, e.TryExpectCreations(key, 1))

	e.DeletionObserved(key, uid1)
	require.False(t, e.IsPendingDeletion(key, uid1))
	require.False(t, e.SatisfiedExpectations(key))
	e.DeletionObserved(key, uid2)
	require.True(t, e.SatisfiedExpectations(key))

	// Observing an unknown or already-cleared UID is a no-op.
	e.DeletionObserved(key, uid1)
	require.True(t, e.SatisfiedExpectations(key))
}

func TestWarmPoolExpectations_EmptyUIDIgnored(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	// Empty UIDs cannot be matched to watch delete events; recording them
	// would collide in the pending set and wedge the pool.
	e.ExpectDeletion(key, "")
	require.True(t, e.SatisfiedExpectations(key))
	require.False(t, e.IsPendingDeletion(key, ""))
}

func TestWarmPoolExpectations_Timeout(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	now := time.Now()
	e.now = func() time.Time { return now }

	require.True(t, e.TryExpectCreations(key, 2))
	e.ExpectDeletion(key, "uid-1")
	require.False(t, e.SatisfiedExpectations(key))

	// Just inside the timeout window: still blocking.
	now = now.Add(expectationsTimeout)
	require.False(t, e.SatisfiedExpectations(key))

	// Past the timeout: the conservative fallback trusts the cache again so a
	// lost watch event cannot wedge the pool forever.
	now = now.Add(time.Second)
	require.True(t, e.SatisfiedExpectations(key))
	require.False(t, e.IsPendingDeletion(key, "uid-1"), "expired records must not keep sandboxes counted as terminating")
	require.True(t, e.TryExpectCreations(key, 1), "new raises are allowed after expiry")
}

func TestWarmPoolExpectations_TimeoutRefreshedByNewRaises(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	now := time.Now()
	e.now = func() time.Time { return now }

	require.True(t, e.TryExpectCreations(key, 1))
	now = now.Add(expectationsTimeout - time.Minute)
	// A new deletion raise refreshes the record's timestamp.
	e.ExpectDeletion(key, "uid-1")
	now = now.Add(2 * time.Minute) // creation raise is now past its original window
	require.False(t, e.SatisfiedExpectations(key), "record must live expectationsTimeout past the LAST raise")
}

func TestWarmPoolExpectations_Forget(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")
	other := expKey("pool-b")

	require.True(t, e.TryExpectCreations(key, 3))
	e.ExpectDeletion(other, "uid-1")

	e.Forget(key)
	require.True(t, e.SatisfiedExpectations(key))
	require.False(t, e.SatisfiedExpectations(other), "Forget must only drop the given pool")
}

// TestWarmPoolExpectations_ConcurrentTryExpect asserts the check-and-record in
// TryExpectCreations is atomic: overlapping reconciles of the same pool can
// never both win the create gate. Run with -race.
func TestWarmPoolExpectations_ConcurrentTryExpect(t *testing.T) {
	e := newWarmPoolExpectations()
	key := expKey("pool-a")

	const goroutines = 64
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Go(func() {
			<-start
			if e.TryExpectCreations(key, 3) {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	require.Equal(t, int32(1), wins.Load(), "exactly one concurrent raise may win")
	require.False(t, e.SatisfiedExpectations(key))
}

// TestWarmPoolExpectations_ConcurrentMixedOps hammers every method from many
// goroutines to catch data races under -race; correctness of the end state is
// asserted only loosely (no deadlock, satisfied after a final Forget).
func TestWarmPoolExpectations_ConcurrentMixedOps(t *testing.T) {
	e := newWarmPoolExpectations()
	keys := []types.NamespacedName{expKey("pool-a"), expKey("pool-b"), expKey("pool-c")}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := keys[n%len(keys)]
			uid := types.UID("uid-" + key.Name)
			for j := range 500 {
				switch j % 6 {
				case 0:
					e.TryExpectCreations(key, 2)
				case 1:
					e.CreationObserved(key)
				case 2:
					e.ExpectDeletion(key, uid)
				case 3:
					e.DeletionObserved(key, uid)
				case 4:
					e.IsPendingDeletion(key, uid)
				case 5:
					e.SatisfiedExpectations(key)
				}
			}
		}(i)
	}
	wg.Wait()

	for _, key := range keys {
		e.Forget(key)
		require.True(t, e.SatisfiedExpectations(key))
	}
}
