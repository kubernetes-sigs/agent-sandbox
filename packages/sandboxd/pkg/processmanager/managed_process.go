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
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ManagedProcess wraps a running OS process, tracking its virtual ID, PTY handles,
// stdin writer, cancel context, and exit state.
type ManagedProcess struct {
	ID        int32
	Cmd       *exec.Cmd
	PTY       *os.File // Non-nil if a PTY was allocated
	Stdin     io.WriteCloser
	Cancel    context.CancelFunc
	ExitCode  int32
	Done      chan struct{}
	mu        sync.Mutex
	completed bool
}

func (p *ManagedProcess) SetExitCode(code int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ExitCode = code
	p.completed = true
}

func (p *ManagedProcess) IsCompleted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completed
}

func (p *ManagedProcess) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed || p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}
	// Send signal to process group if possible (-pid), fallback to single pid
	pid := p.Cmd.Process.Pid
	if err := syscall.Kill(-pid, sig); err != nil {
		return p.Cmd.Process.Signal(sig)
	}
	return nil
}
