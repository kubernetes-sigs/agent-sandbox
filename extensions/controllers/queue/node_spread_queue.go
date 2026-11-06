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

import "container/heap"

// GetWithNodeSpread removes a scheduled sandbox from the node with the most
// queued sandboxes, breaking ties by FIFO order. It uses FIFO when all entries
// are unscheduled. Queues without an index use their existing strategy callback.
func GetWithNodeSpread(q SandboxQueue, namespacedWarmPoolName string) (SandboxKey, bool) {
	if indexed, ok := q.(interface {
		GetWithNodeSpread(string) (SandboxKey, bool)
	}); ok {
		return indexed.GetWithNodeSpread(namespacedWarmPoolName)
	}
	return q.GetWithStrategy(namespacedWarmPoolName, pickNodeSpread)
}

// GetWithNodeSpread selects and removes a sandbox using the per-node index.
func (s *SimpleSandboxQueue) GetWithNodeSpread(namespacedWarmPoolName string) (SandboxKey, bool) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		return SandboxKey{}, false
	}
	sq := q.(*synchronizedQueue)
	sq.mu.Lock()
	defer sq.mu.Unlock()

	if sq.items.Len() == 0 {
		return SandboxKey{}, false
	}
	if len(sq.rankedNodes) > 0 {
		return sq.remove(sq.rankedNodes[0].entries[0]), true
	}
	return sq.remove(sq.items.Front().Value.(*queueEntry)), true
}

func pickNodeSpread(keys []SandboxKey) (SandboxKey, bool) {
	if len(keys) == 0 {
		return SandboxKey{}, false
	}
	counts := make(map[string]int)
	for _, key := range keys {
		if key.NodeName != "" {
			counts[key.NodeName]++
		}
	}
	best := keys[0]
	for _, key := range keys[1:] {
		if counts[key.NodeName] > counts[best.NodeName] {
			best = key
		}
	}
	return best, true
}

// Each node keeps its oldest entry at the head even when an older sandbox
// migrates to it. FIFO append alone would change tie-breaking after migration.
type nodeQueue struct {
	name    string
	entries entryHeap
	index   int
}

func (q *synchronizedQueue) addToNode(entry *queueEntry) {
	if entry.key.NodeName == "" {
		return
	}
	node := q.nodes[entry.key.NodeName]
	if node == nil {
		node = &nodeQueue{name: entry.key.NodeName, index: -1}
		q.nodes[node.name] = node
	}
	heap.Push(&node.entries, entry)
	if node.index < 0 {
		heap.Push(&q.rankedNodes, node)
	} else {
		heap.Fix(&q.rankedNodes, node.index)
	}
}

func (q *synchronizedQueue) removeFromNode(entry *queueEntry) {
	if entry.key.NodeName == "" {
		return
	}
	node := q.nodes[entry.key.NodeName]
	heap.Remove(&node.entries, entry.nodeIndex)
	if len(node.entries) == 0 {
		heap.Remove(&q.rankedNodes, node.index)
		delete(q.nodes, node.name)
	} else {
		heap.Fix(&q.rankedNodes, node.index)
	}
}

type entryHeap []*queueEntry

func (h *entryHeap) Len() int { return len(*h) }

func (h *entryHeap) Less(i, j int) bool { return (*h)[i].sequence < (*h)[j].sequence }

func (h *entryHeap) Swap(i, j int) {
	(*h)[i], (*h)[j] = (*h)[j], (*h)[i]
	(*h)[i].nodeIndex = i
	(*h)[j].nodeIndex = j
}

func (h *entryHeap) Push(value any) {
	entry := value.(*queueEntry)
	entry.nodeIndex = len(*h)
	*h = append(*h, entry)
}

func (h *entryHeap) Pop() any {
	last := len(*h) - 1
	entry := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	// Bound retained storage when most of a node's inventory migrates away.
	if last > 0 && cap(*h) > 64 && last < cap(*h)/4 {
		entries := make(entryHeap, last)
		copy(entries, *h)
		*h = entries
	}
	entry.nodeIndex = -1
	return entry
}

// Node priority follows remaining inventory, then the earliest FIFO position.
type nodeHeap []*nodeQueue

func (h *nodeHeap) Len() int { return len(*h) }

func (h *nodeHeap) Less(i, j int) bool {
	if len((*h)[i].entries) != len((*h)[j].entries) {
		return len((*h)[i].entries) > len((*h)[j].entries)
	}
	return (*h)[i].entries[0].sequence < (*h)[j].entries[0].sequence
}

func (h *nodeHeap) Swap(i, j int) {
	(*h)[i], (*h)[j] = (*h)[j], (*h)[i]
	(*h)[i].index = i
	(*h)[j].index = j
}

func (h *nodeHeap) Push(value any) {
	node := value.(*nodeQueue)
	node.index = len(*h)
	*h = append(*h, node)
}

func (h *nodeHeap) Pop() any {
	last := len(*h) - 1
	node := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	node.index = -1
	return node
}
