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
	"container/list"
	"sync"
)

// SandboxKey uniquely identifies a sandbox in the queue.
type SandboxKey struct {
	Namespace string
	Name      string
	NodeName  string
}

// SandboxQueue defines the interface for managing a thread-safe,
// highly concurrent queue of adoptable warm pool sandboxes.
type SandboxQueue interface {
	Add(namespacedWarmPoolName string, item SandboxKey)
	Get(namespacedWarmPoolName string) (SandboxKey, bool)
	GetWithStrategy(namespacedWarmPoolName string, pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool)
	RemoveQueue(namespacedWarmPoolName string)
	RemoveItem(namespacedWarmPoolName string, item SandboxKey)
}

// SimpleSandboxQueue implements SandboxQueue with per-pool synchronization.
type SimpleSandboxQueue struct {
	// queues is a thread-safe dictionary from warm pool name to a synchronizedQueue
	queues sync.Map
}

// NewSimpleSandboxQueue initializes a new SimpleSandboxQueue.
func NewSimpleSandboxQueue() *SimpleSandboxQueue {
	return &SimpleSandboxQueue{}
}

// Add pushes an item to the specific warm pool's queue.
func (s *SimpleSandboxQueue) Add(namespacedWarmPoolName string, item SandboxKey) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		q, _ = s.queues.LoadOrStore(namespacedWarmPoolName, newSynchronizedQueue())
	}
	q.(*synchronizedQueue).Push(item)
}

// Get pops an item from the specific warm pool's queue.
func (s *SimpleSandboxQueue) Get(namespacedWarmPoolName string) (SandboxKey, bool) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		return SandboxKey{}, false
	}
	return q.(*synchronizedQueue).Pop()
}

// GetWithStrategy pops an item from the specific warm pool's queue using a custom strategy.
func (s *SimpleSandboxQueue) GetWithStrategy(namespacedWarmPoolName string, pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		return SandboxKey{}, false
	}
	return q.(*synchronizedQueue).PopWithStrategy(pick)
}

// RemoveItem deletes a specific sandbox from a warm pool's queue.
func (s *SimpleSandboxQueue) RemoveItem(namespacedWarmPoolName string, item SandboxKey) {
	if q, ok := s.queues.Load(namespacedWarmPoolName); ok {
		sq := q.(*synchronizedQueue)
		sq.Remove(item)
	}
}

// Remove deletes an item by namespace and name.
func (q *synchronizedQueue) Remove(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if entry := q.set[sandboxID{key.Namespace, key.Name}]; entry != nil {
		q.remove(entry)
	}
}

type sandboxID struct {
	namespace string
	name      string
}

type queueEntry struct {
	key       SandboxKey
	sequence  uint64
	element   *list.Element
	nodeIndex int
}

// All indexes share the per-pool lock so selection and removal are atomic.
type synchronizedQueue struct {
	mu           sync.Mutex
	items        list.List
	set          map[sandboxID]*queueEntry
	nodes        map[string]*nodeQueue
	rankedNodes  nodeHeap
	nextSequence uint64
}

func newSynchronizedQueue() *synchronizedQueue {
	return &synchronizedQueue{
		set:   make(map[sandboxID]*queueEntry),
		nodes: make(map[string]*nodeQueue),
	}
}

// Push updates node placement without changing an existing item's FIFO position.
func (q *synchronizedQueue) Push(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()

	id := sandboxID{key.Namespace, key.Name}
	if entry := q.set[id]; entry != nil {
		if entry.key.NodeName != key.NodeName {
			q.removeFromNode(entry)
			entry.key.NodeName = key.NodeName
			q.addToNode(entry)
		}
		return
	}

	if q.nextSequence == ^uint64(0) {
		// Rebase before rollover. Relative order, including heap order, is unchanged.
		q.nextSequence = 0
		for item := q.items.Front(); item != nil; item = item.Next() {
			item.Value.(*queueEntry).sequence = q.nextSequence
			q.nextSequence++
		}
	}
	entry := &queueEntry{key: key, sequence: q.nextSequence, nodeIndex: -1}
	q.nextSequence++
	entry.element = q.items.PushBack(entry)
	q.set[id] = entry
	q.addToNode(entry)
}

// Pop removes and returns the first item from the queue.
func (q *synchronizedQueue) Pop() (SandboxKey, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.items.Len() == 0 {
		return SandboxKey{}, false
	}

	return q.remove(q.items.Front().Value.(*queueEntry)), true
}

// PopWithStrategy applies the strategy function to pick an item from the queue,
// removes it thread-safely, and returns it.
func (q *synchronizedQueue) PopWithStrategy(pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool) {
	for {
		q.mu.Lock()
		if q.items.Len() == 0 {
			q.mu.Unlock()
			return SandboxKey{}, false
		}

		snapshot := make([]SandboxKey, 0, q.items.Len())
		for item := q.items.Front(); item != nil; item = item.Next() {
			snapshot = append(snapshot, item.Value.(*queueEntry).key)
		}
		q.mu.Unlock()

		key, ok := pick(snapshot)
		if !ok {
			return SandboxKey{}, false
		}

		q.mu.Lock()
		entry := q.set[sandboxID{key.Namespace, key.Name}]
		if entry == nil {
			// Another consumer removed the selected key while pick was running.
			q.mu.Unlock()
			continue
		}

		q.remove(entry)
		q.mu.Unlock()

		return key, true
	}
}

// remove updates all indexes while the caller holds q.mu.
func (q *synchronizedQueue) remove(entry *queueEntry) SandboxKey {
	key := entry.key
	q.removeFromNode(entry)
	q.items.Remove(entry.element)
	delete(q.set, sandboxID{key.Namespace, key.Name})
	entry.element.Value = nil
	entry.element = nil
	entry.key = SandboxKey{}
	if q.items.Len() == 0 {
		q.nextSequence = 0
	}
	return key
}

// RemoveQueue completely deletes a warm pool's queue from the sync.Map
// to prevent memory leaks when SandboxTemplates or WarmPools are deleted.
func (s *SimpleSandboxQueue) RemoveQueue(namespacedWarmPoolName string) {
	s.queues.Delete(namespacedWarmPoolName)
}

// GetNamespacedWarmPoolName forms the namespace-aware index value to use as a key to a SimpleSandboxQueue type.
func GetNamespacedWarmPoolName(namespace, warmPoolName string) string {
	return namespace + "/" + warmPoolName
}
