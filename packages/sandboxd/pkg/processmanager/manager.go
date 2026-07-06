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
	pidCounter int32
}

func NewProcessRegistry() *ProcessRegistry {
	return &ProcessRegistry{
		processes:  make(map[int32]*ManagedProcess),
		pidCounter: 0,
	}
}

// NextPID generates a monotonically incrementing virtual process ID starting at 1.
func (r *ProcessRegistry) NextPID() int32 {
	return atomic.AddInt32(&r.pidCounter, 1)
}

func (r *ProcessRegistry) Register(p *ManagedProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processes[p.ID] = p
}

func (r *ProcessRegistry) Get(pid int32) (*ManagedProcess, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.processes[pid]
	return p, ok
}

func (r *ProcessRegistry) Remove(pid int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.processes, pid)
}

func (r *ProcessRegistry) ListActive() []*ManagedProcess {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]*ManagedProcess, 0, len(r.processes))
	for _, p := range r.processes {
		list = append(list, p)
	}
	return list
}

// SignalAll sends a signal to all currently active processes in the registry.
func (r *ProcessRegistry) SignalAll(sig syscall.Signal) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.processes {
		_ = p.Signal(sig)
	}
}
