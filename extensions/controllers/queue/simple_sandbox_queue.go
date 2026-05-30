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
	"strings"
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
func (s *SimpleSandboxQueue) Get(namespacedTemplateHash string) (SandboxKey, bool) {
	if q, ok := s.queues.Load(namespacedTemplateHash); ok {
		return q.(*synchronizedQueue).Pop()
	}

	// Remain compatible with the legacy namespace-agnostic hash
	if templateRefHash, ok := GetTemplateRefHashIfNamespaced(namespacedTemplateHash); ok {
		if q, ok := s.queues.Load(templateRefHash); ok {
			return q.(*synchronizedQueue).Pop()
		}
	}

	return SandboxKey{}, false
}

// RemoveItem deletes a specific sandbox from a template's queue.
func (s *SimpleSandboxQueue) RemoveItem(namespacedTemplateHash string, item SandboxKey) {
	if q, ok := s.queues.Load(namespacedTemplateHash); ok {
		sq := q.(*synchronizedQueue)
		sq.Remove(item)
		return
	}
	// Remain compatible with the legacy namespace-agnostic hash
	if templateRefHash, ok := GetTemplateRefHashIfNamespaced(namespacedTemplateHash); ok {
		if q, ok := s.queues.Load(templateRefHash); ok {
			sq := q.(*synchronizedQueue)
			sq.Remove(item)
		}
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
			// Shift left and clear the tail slot so removed keys don't linger.
			// Same pattern as Pop()
			last := len(q.items) - 1
			copy(q.items[i:], q.items[i+1:])
			q.items[last] = SandboxKey{}
			q.items = q.items[:last]
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
func (s *SimpleSandboxQueue) RemoveQueue(namespacedTemplateHash string) {
	if _, ok := s.queues.Load(namespacedTemplateHash); ok {
		s.queues.Delete(namespacedTemplateHash)
		return
	}
	// Remain compatible with the legacy namespace-agnostic hash
	if templateRefHash, ok := GetTemplateRefHashIfNamespaced(namespacedTemplateHash); ok {
		if _, ok := s.queues.Load(templateRefHash); ok {
			s.queues.Delete(templateRefHash)
		}
	}
}

// GetNamespacedTemplateHash forms the namespace-aware hash value to use as a key to a SimpleSandboxQueue type.
func GetNamespacedTemplateHash(namespace, templateRefHash string) string {
	return namespace + "/" + templateRefHash
}

// GetTemplateRefHashIfNamespaced extracts the templateRefHash value if the parameter is namespace-aware.
func GetTemplateRefHashIfNamespaced(templateHash string) (string, bool) {
	parts := strings.SplitN(templateHash, "/", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}
