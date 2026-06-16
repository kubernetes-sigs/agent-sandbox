// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package queue

import (
	"testing"
)

func TestSimpleSandboxQueue_BasicOperations(t *testing.T) {
	q := NewSimpleSandboxQueue()
	hash := "template-hash-1"

	key1 := SandboxKey{Namespace: "default", Name: "sb-1"}
	key2 := SandboxKey{Namespace: "default", Name: "sb-2"}

	// Test Add
	q.Add(hash, key1)
	q.Add(hash, key2)

	// Test Get (Should be FIFO)
	got1, ok1 := q.Get(hash)
	if !ok1 || got1 != key1 {
		t.Errorf("Expected %v, got %v (ok: %v)", key1, got1, ok1)
	}

	got2, ok2 := q.Get(hash)
	if !ok2 || got2 != key2 {
		t.Errorf("Expected %v, got %v (ok: %v)", key2, got2, ok2)
	}

	// Queue should now be empty
	_, ok3 := q.Get(hash)
	if ok3 {
		t.Errorf("Expected queue to be empty, but got an item")
	}
}

func TestSimpleSandboxQueue_Get_FallbackWhenNamespacedQueueExistsButIsEmpty(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyIndex := "index-1"
	namespace := "my-ns"
	namespacedIndex := GetNamespacedWarmPoolName(namespace, legacyIndex)

	keyLegacy := SandboxKey{Namespace: namespace, Name: "sb-legacy"}
	keyNamespaced := SandboxKey{Namespace: namespace, Name: "sb-namespaced"}

	// Add to legacy queue
	q.Add(legacyIndex, keyLegacy)

	// Add to namespaced queue to make it exist
	q.Add(namespacedIndex, keyNamespaced)

	// Pop from namespaced queue to make it empty
	got, ok := q.Get(namespacedIndex)
	if !ok || got != keyNamespaced {
		t.Fatalf("Setup failed: expected to pop %v from namespaced queue, got %v (ok: %v)", keyNamespaced, got, ok)
	}

	// Now the namespaced queue exists but is empty.
	// Get should fallback to the legacy queue and return keyLegacy.
	gotFallback, okFallback := q.Get(namespacedIndex)
	if !okFallback {
		t.Errorf("Expected to get item from legacy queue fallback, but got !ok")
	}
	if gotFallback != keyLegacy {
		t.Errorf("Expected %v from legacy queue fallback, got %v", keyLegacy, gotFallback)
	}
}

func TestSimpleSandboxQueue_RemoveItem_GhostPodFix(t *testing.T) {
	q := NewSimpleSandboxQueue()
	hash := "template-hash-1"

	key1 := SandboxKey{Namespace: "default", Name: "sb-1"}
	key2 := SandboxKey{Namespace: "default", Name: "sb-2"}
	key3 := SandboxKey{Namespace: "default", Name: "sb-3"}

	q.Add(hash, key1)
	q.Add(hash, key2)
	q.Add(hash, key3)

	// Simulate the Kubelet deleting the middle pod (Ghost Pod scenario)
	q.RemoveItem(hash, key2)

	// Ensure RemoveItem does not retain stale references in backing array tail.
	rawQueue, ok := q.queues.Load(hash)
	if !ok {
		t.Fatalf("Expected queue for %q to exist", hash)
	}
	sq := rawQueue.(*synchronizedQueue)
	if cap(sq.items) > len(sq.items) {
		backing := sq.items[:cap(sq.items)]
		for i := len(sq.items); i < len(backing); i++ {
			if backing[i] != (SandboxKey{}) {
				t.Errorf("Expected backing array slot %d to be cleared, found %+v", i, backing[i])
			}
		}
	}

	// First pop should still be key1
	got1, _ := q.Get(hash)
	if got1 != key1 {
		t.Errorf("Expected %v, got %v", key1, got1)
	}

	// Second pop should be key3! (key2 was successfully removed)
	got3, _ := q.Get(hash)
	if got3 != key3 {
		t.Errorf("Expected %v to skip deleted item and return %v, but got %v", hash, key3, got3)
	}

	// Queue should now be empty
	_, hasItem := q.Get(hash)
	if hasItem {
		t.Errorf("Expected queue to be empty after Ghost Pod removal")
	}
}

func TestSynchronizedQueue_Deduplication(t *testing.T) {
	q := newSynchronizedQueue()
	key := SandboxKey{Namespace: "default", Name: "duplicate-sb"}

	// Push the exact same pod 3 times
	q.Push(key)
	q.Push(key)
	q.Push(key)

	// Verify it only stored it once
	if len(q.items) != 1 {
		t.Errorf("Expected length 1 due to O(1) deduplication, got %d", len(q.items))
	}

	// Verify the set also only has 1 item
	if len(q.set) != 1 {
		t.Errorf("Expected set length 1, got %d", len(q.set))
	}
}

func TestSimpleSandboxQueue_RemoveQueue_MemoryLeakFix(t *testing.T) {
	q := NewSimpleSandboxQueue()
	hash := "template-hash-to-delete"
	key1 := SandboxKey{Namespace: "default", Name: "sb-1"}

	q.Add(hash, key1)

	// Simulate SandboxTemplate deletion
	q.RemoveQueue(hash)

	// Verify the entire queue was wiped from the sync.Map
	_, ok := q.Get(hash)
	if ok {
		t.Errorf("Expected queue to be completely removed, but it still existed")
	}
}

func TestSimpleSandboxQueue_GetWithStrategy(t *testing.T) {
	q := NewSimpleSandboxQueue()
	hash := "template-hash-1"

	key1 := SandboxKey{Namespace: "default", Name: "sb-1"}
	key2 := SandboxKey{Namespace: "default", Name: "sb-2"}
	key3 := SandboxKey{Namespace: "default", Name: "sb-3"}

	q.Add(hash, key1)
	q.Add(hash, key2)
	q.Add(hash, key3)

	// Custom strategy to pick key2 specifically
	pickKey2 := func(items []SandboxKey) (SandboxKey, bool) {
		for _, item := range items {
			if item.Name == "sb-2" {
				return item, true
			}
		}
		return SandboxKey{}, false
	}

	// Pop with strategy
	got, ok := q.GetWithStrategy(hash, pickKey2)
	if !ok || got != key2 {
		t.Errorf("Expected to pick %v, got %v (ok: %v)", key2, got, ok)
	}

	// First standard pop should be key1 (since key2 was removed)
	got1, _ := q.Get(hash)
	if got1 != key1 {
		t.Errorf("Expected first remaining item to be %v, got %v", key1, got1)
	}

	// Second standard pop should be key3
	got3, _ := q.Get(hash)
	if got3 != key3 {
		t.Errorf("Expected second remaining item to be %v, got %v", key3, got3)
	}

	// Queue should now be empty
	_, ok3 := q.Get(hash)
	if ok3 {
		t.Errorf("Expected queue to be empty, but got an item")
	}
}

func TestSimpleSandboxQueue_GetWithStrategyFallback(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyIndex := "my-legacy-index"
	namespace := "my-ns"
	namespacedWarmPoolName := GetNamespacedWarmPoolName(namespace, legacyIndex)

	key1 := SandboxKey{Namespace: namespace, Name: "sb-1"}
	key2 := SandboxKey{Namespace: namespace, Name: "sb-2"}

	// 1. Add items to the legacy queue
	q.Add(legacyIndex, key1)
	q.Add(legacyIndex, key2)

	// Custom strategy to pick key2 specifically
	pickKey2 := func(items []SandboxKey) (SandboxKey, bool) {
		for _, item := range items {
			if item.Name == "sb-2" {
				return item, true
			}
		}
		return SandboxKey{}, false
	}

	// 2. Call GetWithStrategy with namespaced index (which should fall back to legacy)
	got, ok := q.GetWithStrategy(namespacedWarmPoolName, pickKey2)
	if !ok || got != key2 {
		t.Errorf("Expected to pick %v via fallback, got %v (ok: %v)", key2, got, ok)
	}

	// 3. Verify key2 was removed from legacy queue by checking remaining items
	// Standard Get with namespaced index should still fall back to legacy and get key1
	got1, ok1 := q.Get(namespacedWarmPoolName)
	if !ok1 || got1 != key1 {
		t.Errorf("Expected to get %v via fallback, got %v (ok: %v)", key1, got1, ok1)
	}

	// Queue should now be empty
	_, okEmpty := q.Get(namespacedWarmPoolName)
	if okEmpty {
		t.Errorf("Expected queue to be empty")
	}
}

func TestSimpleSandboxQueue_KeyFallbackBehavior(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyIndex := "my-index-1"
	namespace := "my-ns"
	namespacedWarmPoolName := GetNamespacedWarmPoolName(namespace, legacyIndex)

	key1 := SandboxKey{Namespace: namespace, Name: "sb-1"}

	// Test that the namespace-agnostic legacy value can still be referenced by using the namespace-aware value to interact with the queues
	q.Add(legacyIndex, key1)
	got1, ok := q.Get(namespacedWarmPoolName)
	if !ok || got1 != key1 {
		t.Errorf("Expected %v from Get fallback, got %v (ok: %v)", key1, got1, ok)
	}

	// Test RemoveItem fallback
	q.RemoveItem(namespacedWarmPoolName, key1)
	_, ok = q.Get(legacyIndex)
	if ok {
		t.Errorf("Expected legacy queue to be empty after RemoveItem with fallback")
	}

	// Test RemoveQueue fallback (add item back first)
	q.Add(legacyIndex, key1)
	q.RemoveQueue(namespacedWarmPoolName)
	_, ok = q.queues.Load(legacyIndex)
	if ok {
		t.Errorf("Expected legacy queue to be deleted after RemoveQueue with fallback")
	}

	// Test reverse is false: that namespace-aware value cannot be referenced by using the legacy value
	q.Add(namespacedWarmPoolName, key1)
	_, ok = q.Get(legacyIndex)
	if ok {
		t.Errorf("Expected legacy Get to NOT find namespaced item")
	}

	// Test RemoveItem does nothing with legacy index
	q.RemoveItem(legacyIndex, key1)
	got2, ok := q.Get(namespacedWarmPoolName)
	if !ok || got2 != key1 {
		t.Errorf("Expected item %v to still be in queue after RemoveItem with legacy index, got %v", key1, got2)
	}

	// Test RemoveQueue does nothing with legacy index (add item back first)
	q.Add(namespacedWarmPoolName, key1)
	q.RemoveQueue(legacyIndex)
	got3, ok := q.Get(namespacedWarmPoolName)
	if !ok || got3 != key1 {
		t.Errorf("Expected item %v to still be in namespaced queue after RemoveQueue with legacy index, got %v", key1, got3)
	}
}

func TestGetNamespacedWarmPoolName(t *testing.T) {
	namespace := "my-ns"
	wp := "my-wp"
	expected := "my-ns/my-wp"
	got := GetNamespacedWarmPoolName(namespace, wp)
	if got != expected {
		t.Errorf("Expected %q, got %q", expected, got)
	}
}

func TestGetWarmPoolNameIfNamespaced(t *testing.T) {
	testCases := []struct {
		name          string
		input         string
		expectedValue string
		expectedOk    bool
	}{
		{
			name:          "namespace-aware index",
			input:         "my-ns/my-index",
			expectedValue: "my-index",
			expectedOk:    true,
		},
		{
			name:          "namespace-agnostic (legacy) index",
			input:         "my-index",
			expectedValue: "",
			expectedOk:    false,
		},
		{
			name:          "empty string",
			input:         "",
			expectedValue: "",
			expectedOk:    false,
		},
		{
			name:          "unexpected format (multiple slashes)",
			input:         "ns/dir/index",
			expectedValue: "dir/index",
			expectedOk:    true,
		},
		{
			name:          "malformed (nothing after namespace)",
			input:         "ns/",
			expectedValue: "",
			expectedOk:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			warmPoolName, ok := GetWarmPoolNameIfNamespaced(tc.input)
			if ok != tc.expectedOk {
				t.Errorf("Expected ok %v, got %v", tc.expectedOk, ok)
			}
			if warmPoolName != tc.expectedValue {
				t.Errorf("Expected warmPoolName %q, got %q", tc.expectedValue, warmPoolName)
			}
		})
	}
}

func TestSimpleSandboxQueue_RemoveItem_BothQueuesExist(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyIndex := "template-index-1"
	namespace := "my-ns"
	namespacedIndex := GetNamespacedWarmPoolName(namespace, legacyIndex)

	key := SandboxKey{Namespace: namespace, Name: "sb-1"}

	// Add the same item to both queues
	q.Add(legacyIndex, key)
	q.Add(namespacedIndex, key)

	// Remove the item using namespacedIndex.
	// It should be removed from both namespaced and legacy queues.
	q.RemoveItem(namespacedIndex, key)

	// Verify legacy queue is empty
	_, ok := q.Get(legacyIndex)
	if ok {
		t.Errorf("Expected legacy queue to be empty after RemoveItem")
	}

	// Verify namespaced queue is also empty
	_, ok = q.Get(namespacedIndex)
	if ok {
		t.Errorf("Expected namespaced queue to be empty after RemoveItem")
	}
}

func TestSimpleSandboxQueue_RemoveQueue_BothQueuesExist(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyIndex := "template-index-1"
	namespace := "my-ns"
	namespacedIndex := GetNamespacedWarmPoolName(namespace, legacyIndex)

	keyLegacy := SandboxKey{Namespace: namespace, Name: "sb-legacy"}
	keyNamespaced := SandboxKey{Namespace: namespace, Name: "sb-namespaced"}

	// Add to both queues
	q.Add(legacyIndex, keyLegacy)
	q.Add(namespacedIndex, keyNamespaced)

	// Remove namespaced queue. It should also remove legacy queue.
	q.RemoveQueue(namespacedIndex)

	// Verify both are removed from sync.Map
	_, ok := q.queues.Load(namespacedIndex)
	if ok {
		t.Errorf("Expected namespaced queue to be deleted")
	}
	_, ok = q.queues.Load(legacyIndex)
	if ok {
		t.Errorf("Expected legacy queue to be deleted")
	}
}
