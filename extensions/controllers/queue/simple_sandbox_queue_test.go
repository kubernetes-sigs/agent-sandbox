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

func TestSimpleSandboxQueue_KeyFallbackBehavior(t *testing.T) {
	q := NewSimpleSandboxQueue()
	legacyHash := "template-hash-1"
	namespace := "my-ns"
	namespacedHash := GetNamespacedTemplateHash(namespace, legacyHash)

	key1 := SandboxKey{Namespace: namespace, Name: "sb-1"}

	// Test that the namespace-agnostic legacy value can still be referenced by using the namespace-aware value to interact with the queues
	q.Add(legacyHash, key1)
	got1, ok := q.Get(namespacedHash)
	if !ok || got1 != key1 {
		t.Errorf("Expected %v from Get fallback, got %v (ok: %v)", key1, got1, ok)
	}

	// Test RemoveItem fallback
	q.RemoveItem(namespacedHash, key1)
	_, ok = q.Get(legacyHash)
	if ok {
		t.Errorf("Expected legacy queue to be empty after RemoveItem with fallback")
	}

	// Test RemoveQueue fallback (add item back first)
	q.Add(legacyHash, key1)
	q.RemoveQueue(namespacedHash)
	_, ok = q.queues.Load(legacyHash)
	if ok {
		t.Errorf("Expected legacy queue to be deleted after RemoveQueue with fallback")
	}

	// Test reverse is false: that namespace-aware value cannot be referenced by using the legacy value
	q.Add(namespacedHash, key1)
	_, ok = q.Get(legacyHash)
	if ok {
		t.Errorf("Expected legacy Get to NOT find namespaced item")
	}

	// Test RemoveItem does nothing with legacy hash
	q.RemoveItem(legacyHash, key1)
	got2, ok := q.Get(namespacedHash)
	if !ok || got2 != key1 {
		t.Errorf("Expected item %v to still be in queue after RemoveItem with legacy hash, got %v", key1, got2)
	}

	// Test RemoveQueue does nothing with legacy hash (add item back first)
	q.Add(namespacedHash, key1)
	q.RemoveQueue(legacyHash)
	got3, ok := q.Get(namespacedHash)
	if !ok || got3 != key1 {
		t.Errorf("Expected item %v to still be in namespaced queue after RemoveQueue with legacy hash, got %v", key1, got3)
	}
}

func TestGetNamespacedTemplateHash(t *testing.T) {
	namespace := "my-ns"
	hash := "my-hash"
	expected := "my-ns/my-hash"
	got := GetNamespacedTemplateHash(namespace, hash)
	if got != expected {
		t.Errorf("Expected %q, got %q", expected, got)
	}
}

func TestGetTemplateRefHashIfNamespaced(t *testing.T) {
	testCases := []struct {
		name         string
		input        string
		expectedHash string
		expectedOk   bool
	}{
		{
			name:         "namespace-aware hash",
			input:        "my-ns/my-hash",
			expectedHash: "my-hash",
			expectedOk:   true,
		},
		{
			name:         "namespace-agnostic (legacy) hash",
			input:        "my-hash",
			expectedHash: "",
			expectedOk:   false,
		},
		{
			name:         "empty string",
			input:        "",
			expectedHash: "",
			expectedOk:   false,
		},
		{
			name:         "unexpected format (multiple slashes)",
			input:        "ns/dir/hash",
			expectedHash: "dir/hash",
			expectedOk:   true,
		},
		{
			name:         "malformed (nothing after namespace)",
			input:        "ns/",
			expectedHash: "",
			expectedOk:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hash, ok := GetTemplateRefHashIfNamespaced(tc.input)
			if ok != tc.expectedOk {
				t.Errorf("Expected ok %v, got %v", tc.expectedOk, ok)
			}
			if hash != tc.expectedHash {
				t.Errorf("Expected hash %q, got %q", tc.expectedHash, hash)
			}
		})
	}
}
