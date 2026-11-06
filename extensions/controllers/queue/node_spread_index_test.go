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

package queue

import (
	"fmt"
	"testing"
)

func TestNodeSpreadIndexMutations(t *testing.T) {
	q := newSynchronizedQueue()
	keys := make([]SandboxKey, 96)
	for i := range keys {
		keys[i] = SandboxKey{Namespace: "default", Name: fmt.Sprintf("sandbox-%d", i), NodeName: fmt.Sprintf("node-%d", i%4)}
		q.Push(keys[i])
		assertIndexedQueueConsistent(t, q)
	}
	for i := range keys {
		if i%2 == 0 {
			keys[i].NodeName = ""
		} else {
			keys[i].NodeName = "migrated"
		}
		q.Push(keys[i])
		assertIndexedQueueConsistent(t, q)
	}
	for i, key := range keys {
		switch i % 3 {
		case 0:
			q.Remove(key)
		case 1:
			q.Pop()
		case 2:
			q.PopWithStrategy(func(items []SandboxKey) (SandboxKey, bool) {
				return items[len(items)-1], true
			})
		}
		assertIndexedQueueConsistent(t, q)
	}
	for q.items.Len() > 0 {
		q.Pop()
		assertIndexedQueueConsistent(t, q)
	}
	if len(q.nodes) != 0 || len(q.rankedNodes) != 0 || len(q.set) != 0 {
		t.Fatal("empty queue retains index entries")
	}
}

func TestNodeSpreadIndexShrinksAfterMigration(t *testing.T) {
	q := newSynchronizedQueue()
	for i := range 1024 {
		q.Push(SandboxKey{Namespace: "default", Name: fmt.Sprintf("sandbox-%d", i), NodeName: "original"})
	}
	for i := range 1023 {
		q.Push(SandboxKey{Namespace: "default", Name: fmt.Sprintf("sandbox-%d", i), NodeName: "destination"})
	}
	assertIndexedQueueConsistent(t, q)
	if capacity := cap(q.nodes["original"].entries); capacity > 64 {
		t.Fatalf("one remaining entry retains capacity %d", capacity)
	}
}

func TestNodeSpreadIndexSequenceRollover(t *testing.T) {
	q := NewSimpleSandboxQueue()
	pool := "default/pool"
	keys := []SandboxKey{
		{Namespace: "default", Name: "oldest", NodeName: "node-a"},
		{Namespace: "default", Name: "middle", NodeName: "node-b"},
		{Namespace: "default", Name: "newest", NodeName: "node-a"},
	}
	q.Add(pool, keys[0])
	raw, _ := q.queues.Load(pool)
	sq := raw.(*synchronizedQueue)
	sq.nextSequence = ^uint64(0) - 1
	q.Add(pool, keys[1])
	q.Add(pool, keys[2])
	assertIndexedQueueConsistent(t, sq)
	for _, want := range keys {
		got, ok := q.GetWithNodeSpread(pool)
		if !ok || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, ok, want)
		}
		assertIndexedQueueConsistent(t, sq)
	}
}

func assertIndexedQueueConsistent(t *testing.T, q *synchronizedQueue) {
	t.Helper()
	if q.items.Len() != len(q.set) {
		t.Fatalf("FIFO length %d differs from identity index %d", q.items.Len(), len(q.set))
	}
	if len(q.nodes) != len(q.rankedNodes) {
		t.Fatalf("node map length %d differs from heap %d", len(q.nodes), len(q.rankedNodes))
	}
	counts := make(map[string]int)
	var previous uint64
	for item := q.items.Front(); item != nil; item = item.Next() {
		entry := item.Value.(*queueEntry)
		if entry.element != item || q.set[sandboxID{entry.key.Namespace, entry.key.Name}] != entry {
			t.Fatalf("inconsistent FIFO or identity entry: %+v", entry.key)
		}
		if item.Prev() != nil && entry.sequence <= previous {
			t.Fatalf("sequence %d follows %d", entry.sequence, previous)
		}
		previous = entry.sequence
		if entry.key.NodeName == "" {
			if entry.nodeIndex != -1 {
				t.Fatalf("unscheduled key has node index %d", entry.nodeIndex)
			}
			continue
		}
		counts[entry.key.NodeName]++
		node := q.nodes[entry.key.NodeName]
		if node == nil || entry.nodeIndex < 0 || entry.nodeIndex >= len(node.entries) || node.entries[entry.nodeIndex] != entry {
			t.Fatalf("missing node index for %+v", entry.key)
		}
	}
	for name, node := range q.nodes {
		if len(node.entries) == 0 || len(node.entries) != counts[name] {
			t.Fatalf("node %q has %d entries, want %d", name, len(node.entries), counts[name])
		}
		if node.name != name || node.index < 0 || node.index >= len(q.rankedNodes) || q.rankedNodes[node.index] != node {
			t.Fatalf("inconsistent ranked node %q", name)
		}
		for i, entry := range node.entries {
			if entry.nodeIndex != i || entry.key.NodeName != name {
				t.Fatalf("inconsistent entry %d on node %q", i, name)
			}
			if i > 0 && node.entries.Less(i, (i-1)/2) {
				t.Fatalf("invalid entry heap on node %q", name)
			}
		}
		for _, entry := range node.entries[len(node.entries):cap(node.entries)] {
			if entry != nil {
				t.Fatalf("node %q retains a removed entry", name)
			}
		}
	}
	for i := range q.rankedNodes {
		if i > 0 && q.rankedNodes.Less(i, (i-1)/2) {
			t.Fatal("invalid node inventory heap")
		}
	}
	for _, node := range q.rankedNodes[len(q.rankedNodes):cap(q.rankedNodes)] {
		if node != nil {
			t.Fatal("node heap retains a removed node")
		}
	}
}
