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
		t.Errorf("Expected queue for %q to exist", hash)
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
	_, empty := q.Get(hash)
	if empty {
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

	// Also verify item-by-item removal cleans up empty queues in sync.Map.
	hash2 := "template-hash-item-by-item-delete"
	key2 := SandboxKey{Namespace: "default", Name: "sb-2"}

	q.Add(hash2, key1)
	q.Add(hash2, key2)
	q.RemoveItem(hash2, key1)
	if _, exists := q.queues.Load(hash2); !exists {
		t.Errorf("Expected queue for %q to still exist while items remain", hash2)
	}
	q.RemoveItem(hash2, key2)
	if _, exists := q.queues.Load(hash2); exists {
		t.Errorf("Expected empty queue for %q to be removed from sync.Map", hash2)
	}
}
