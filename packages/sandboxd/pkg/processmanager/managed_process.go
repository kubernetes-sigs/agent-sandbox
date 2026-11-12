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
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ManagedProcess wraps a running OS process, tracking its virtual ID, PTY
// handle, stdin writer, and exit state.
type ManagedProcess struct {
	// ID is the virtual process ID handed to clients. It is decoupled from
	// the OS PID so clients cannot signal arbitrary host processes.
	ID    int32
	Cmd   *exec.Cmd
	PTY   *os.File // Non-nil if a PTY was allocated.
	Stdin *os.File // Pipe writer or PTY master; must support write deadlines.
	// Done is closed once the process has exited and its exit code recorded.
	Done chan struct{}

	mu         sync.Mutex
	exitCode   int32
	completed  bool
	stdinWrite chan struct{}
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

// ErrStdinIsTerminal is returned by CloseStdin for a PTY process, whose
// stdin is the terminal master. Closing it would hang up the terminal
// rather than signal end of input.
var ErrStdinIsTerminal = errors.New("stdin is a PTY")

// WriteStdin writes input until it is delivered or ctx is canceled. A canceled
// write may have delivered part of input; the stdin remains open for later calls.
func (p *ManagedProcess) WriteStdin(ctx context.Context, input []byte) error {
	// Write deadlines belong to the file, so each request must own both
	// its write and cancellation callback before another request can write.
	p.mu.Lock()
	if p.stdinWrite == nil {
		p.stdinWrite = make(chan struct{}, 1)
	}
	gate := p.stdinWrite
	p.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Stdin.SetWriteDeadline(time.Time{}); err != nil {
		return err
	}

	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = p.Stdin.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	_, err := p.Stdin.Write(input)
	if !stop() {
		<-interrupted
	}
	// Wait for cancellation before clearing the deadline, so an old request
	// cannot interrupt the next writer. CloseStdin can run throughout the write.
	_ = p.Stdin.SetWriteDeadline(time.Time{})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CloseStdin closes the process stdin writer if present.
func (p *ManagedProcess) CloseStdin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Stdin == nil {
		return nil
	}
	if p.PTY != nil && p.Stdin == p.PTY {
		return ErrStdinIsTerminal
	}
	return p.Stdin.Close()
}

// ResizeTTY resizes the pseudo-terminal window if active and process is running.
func (p *ManagedProcess) ResizeTTY(cols, rows uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed || p.PTY == nil {
		return errors.New("process is completed or has no active PTY")
	}
	// pty.Setsize calls File.Fd, which switches the master to blocking I/O
	// and prevents write deadlines from interrupting backpressured input.
	conn, err := p.PTY.SyscallConn()
	if err != nil {
		return err
	}
	var resizeErr error
	if err := conn.Control(func(fd uintptr) {
		resizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	}); err != nil {
		return err
	}
	return resizeErr
}

// ClosePTY closes the PTY master handle under lock and clears the reference.
func (p *ManagedProcess) ClosePTY() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.PTY == nil {
		return nil
	}
	err := p.PTY.Close()
	p.PTY = nil
	return err
}
