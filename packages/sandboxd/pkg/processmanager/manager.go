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

package processmanager

import (
	"sync"
	"sync/atomic"
	"syscall"
)

// ProcessRegistry provides thread-safe management of virtual process IDs
// and process handles.
type ProcessRegistry struct {
	mu         sync.RWMutex
	processes  map[int32]*ManagedProcess
	pidCounter atomic.Int32
}

// NewProcessRegistry returns an empty registry.
func NewProcessRegistry() *ProcessRegistry {
	return &ProcessRegistry{
		processes: make(map[int32]*ManagedProcess),
	}
}

// NextPID generates a monotonically incrementing virtual process ID starting at 1.
func (r *ProcessRegistry) NextPID() int32 {
	return r.pidCounter.Add(1)
}

// Register adds p to the registry, keyed by its virtual ID.
func (r *ProcessRegistry) Register(p *ManagedProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processes[p.ID] = p
}

// Get looks up a process by virtual ID.
func (r *ProcessRegistry) Get(pid int32) (*ManagedProcess, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.processes[pid]
	return p, ok
}

// Remove deletes a process from the registry.
func (r *ProcessRegistry) Remove(pid int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.processes, pid)
}

// SignalAll sends a signal to all currently active processes in the registry.
func (r *ProcessRegistry) SignalAll(sig syscall.Signal) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.processes {
		_ = p.Signal(sig)
	}
}
