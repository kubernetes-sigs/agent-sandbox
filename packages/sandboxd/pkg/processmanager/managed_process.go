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

// Package processmanager tracks processes spawned by the sandboxd
// ProcessService, assigning stable virtual IDs decoupled from OS PIDs.
package processmanager

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ManagedProcess wraps a running OS process, tracking its virtual ID, PTY
// handle, stdin writer, and exit state.
type ManagedProcess struct {
	// ID is the virtual process ID handed to clients. It is decoupled from
	// the OS PID so clients cannot signal arbitrary host processes.
	ID    int32
	Cmd   *exec.Cmd
	PTY   *os.File // Non-nil if a PTY was allocated.
	Stdin io.WriteCloser
	// Done is closed once the process has exited and its exit code recorded.
	Done chan struct{}

	mu        sync.Mutex
	exitCode  int32
	completed bool
}

// SetExitCode records the process exit code and marks it completed.
func (p *ManagedProcess) SetExitCode(code int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exitCode = code
	p.completed = true
}

// ExitCode returns the recorded exit code. Only meaningful after Done is closed.
func (p *ManagedProcess) ExitCode() int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// IsCompleted reports whether the process has exited.
func (p *ManagedProcess) IsCompleted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completed
}

// Signal delivers sig to the process group when possible (-pid), falling
// back to the single process. Signalling an already-completed process is a
// no-op so shutdown sweeps don't race with process exit.
func (p *ManagedProcess) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed || p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}
	pid := p.Cmd.Process.Pid
	if err := syscall.Kill(-pid, sig); err != nil {
		return p.Cmd.Process.Signal(sig)
	}
	return nil
}

// CloseStdin closes the process stdin writer if present.
func (p *ManagedProcess) CloseStdin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Stdin == nil {
		return nil
	}
	return p.Stdin.Close()
}
