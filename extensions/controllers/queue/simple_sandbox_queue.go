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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// SandboxKey uniquely identifies a sandbox in the queue.
type SandboxKey types.NamespacedName

// SandboxQueue defines the interface for managing a thread-safe,
// highly concurrent queue of adoptable warm pool sandboxes.
type SandboxQueue interface {
	Add(templateHash string, item SandboxKey)
	Get(templateHash string) (SandboxKey, bool)
	RemoveQueue(templateHash string)
	RemoveItem(templateHash string, item SandboxKey)
}

// SimpleSandboxQueue implements SandboxQueue using simple synchronized slices.
type SimpleSandboxQueue struct {
	// queues is a thread-safe dictionary from template hash to a synchronizedQueue
	queues sync.Map
}

// NewSimpleSandboxQueue initializes a new SimpleSandboxQueue.
func NewSimpleSandboxQueue() *SimpleSandboxQueue {
	return &SimpleSandboxQueue{}
}

// Add pushes an item to the specific template's queue.
func (s *SimpleSandboxQueue) Add(templateHash string, item SandboxKey) {
	q, _ := s.queues.LoadOrStore(templateHash, newSynchronizedQueue())
	q.(*synchronizedQueue).Push(item)
}

// Get pops an item from the specific template's queue.
func (s *SimpleSandboxQueue) Get(templateHash string) (SandboxKey, bool) {
	q, ok := s.queues.Load(templateHash)
	if !ok {
		return SandboxKey{}, false
	}
	return q.(*synchronizedQueue).Pop()
}

// RemoveItem deletes a specific sandbox from a template's queue.
func (s *SimpleSandboxQueue) RemoveItem(templateHash string, item SandboxKey) {
	if q, ok := s.queues.Load(templateHash); ok {
		q.(*synchronizedQueue).Remove(item)
	}
}

// Remove scans the slice and deletes the item to prevent Ghost Pods.
func (q *synchronizedQueue) Remove(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, exists := q.set[key]; !exists {
		return
	}

	delete(q.set, key)

	for i, k := range q.items {
		if k == key {
			// Delete from slice
			q.items = append(q.items[:i], q.items[i+1:]...)
			break
		}
	}
}

// TODO(vicentefb): Implement queue cleanup mechanism.
// We should remove the queue from the sync.Map when the corresponding
// SandboxWarmPool for a given template is deleted to prevent memory leaks.
type synchronizedQueue struct {
	mu    sync.Mutex
	items []SandboxKey
	set   map[SandboxKey]struct{} // Used for O(1) deduplication
}

func newSynchronizedQueue() *synchronizedQueue {
	return &synchronizedQueue{
		items: make([]SandboxKey, 0),
		set:   make(map[SandboxKey]struct{}),
	}
}

// Push adds an item to the queue if it isn't already present.
func (q *synchronizedQueue) Push(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.set[key]; !exists {
		q.set[key] = struct{}{}
		q.items = append(q.items, key)
	}
}

// Pop removes and returns the first item from the queue.
func (q *synchronizedQueue) Pop() (SandboxKey, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return SandboxKey{}, false
	}

	// Grab the first item
	item := q.items[0]

	// This removes the pointer references so the Garbage Collector
	// can free the strings in memory!
	q.items[0] = SandboxKey{}

	// Remove it from slice and set
	q.items = q.items[1:]
	delete(q.set, item)

	return item, true
}

// RemoveQueue completely deletes a template's queue from the sync.Map
// to prevent memory leaks when SandboxTemplates or WarmPools are deleted.
func (s *SimpleSandboxQueue) RemoveQueue(templateHash string) {
	s.queues.Delete(templateHash)
}
