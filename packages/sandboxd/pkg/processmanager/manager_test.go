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
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNextPIDMonotonic(t *testing.T) {
	r := NewProcessRegistry()
	require.Equal(t, int32(1), r.NextPID())
	require.Equal(t, int32(2), r.NextPID())
	require.Equal(t, int32(3), r.NextPID())
}

func TestNextPIDConcurrentUnique(t *testing.T) {
	r := NewProcessRegistry()
	const n = 100
	pids := make(chan int32, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pids <- r.NextPID()
		}()
	}
	wg.Wait()
	close(pids)

	seen := make(map[int32]bool, n)
	for pid := range pids {
		require.False(t, seen[pid], "duplicate pid %d", pid)
		seen[pid] = true
	}
	require.Len(t, seen, n)
}

func TestRegisterGetRemove(t *testing.T) {
	r := NewProcessRegistry()
	p := &ManagedProcess{ID: r.NextPID(), Done: make(chan struct{})}
	r.Register(p)

	got, ok := r.Get(p.ID)
	require.True(t, ok)
	require.Same(t, p, got)

	r.Remove(p.ID)
	_, ok = r.Get(p.ID)
	require.False(t, ok)
}

func TestSignalCompletedProcessIsNoop(t *testing.T) {
	p := &ManagedProcess{ID: 1, Done: make(chan struct{})}
	p.SetExitCode(0)
	require.True(t, p.IsCompleted())
	// Cmd is nil; must not panic and must not error.
	require.NoError(t, p.Signal(syscall.SIGTERM))
}

func TestSignalAllSkipsCompleted(t *testing.T) {
	r := NewProcessRegistry()
	p := &ManagedProcess{ID: r.NextPID(), Done: make(chan struct{})}
	p.SetExitCode(1)
	r.Register(p)
	// No live process behind it: SignalAll must be a safe no-op.
	r.SignalAll(syscall.SIGKILL)
}

func TestExitCodeRoundTrip(t *testing.T) {
	p := &ManagedProcess{ID: 1, Done: make(chan struct{})}
	p.SetExitCode(42)
	require.Equal(t, int32(42), p.ExitCode())
}
