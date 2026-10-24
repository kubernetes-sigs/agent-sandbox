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
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

const (
	// defaultReaperWaitTimeout bounds how long WaitResult waits for the
	// background reaper to record the exit status of a tracked process when
	// cmd.Wait() returned ECHILD because the reaper collected it first.
	defaultReaperWaitTimeout = 5 * time.Second

	// reaperTickInterval is the periodic safety-sweep interval to catch any
	// dead children whose SIGCHLD signals were coalesced by the kernel.
	reaperTickInterval = 1 * time.Second
)

type trackedProcess struct {
	done   chan struct{}
	status syscall.WaitStatus
	once   sync.Once
}

func newTrackedProcess() *trackedProcess {
	return &trackedProcess{
		done: make(chan struct{}),
	}
}

func (tp *trackedProcess) set(ws syscall.WaitStatus) {
	tp.once.Do(func() {
		tp.status = ws
		close(tp.done)
	})
}

type recentExit struct {
	status    syscall.WaitStatus
	timestamp time.Time
}

// Reaper reaps dead child processes in the background when running as PID 1
// (or as a child subreaper), preventing orphaned zombie (<defunct>) processes
// from accumulating in the sandbox container. It coordinates with os/exec by
// tracking actively running child PIDs so that cmd.Wait() does not lose exit
// status if the background reaper reaps the child process first.
type Reaper struct {
	log           logr.Logger
	stopCh        chan struct{}
	doneCh        chan struct{}
	sigCh         chan os.Signal
	waitTimeout   time.Duration
	reapedOrphans atomic.Int64

	mu           sync.Mutex
	reapMu       sync.Mutex
	tracked      map[int]*trackedProcess
	recentReaped map[int]recentExit
	started      bool
	stopped      bool
}

// NewReaper constructs an unstarted Reaper.
func NewReaper(log logr.Logger) *Reaper {
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &Reaper{
		log:          log,
		waitTimeout:  defaultReaperWaitTimeout,
		tracked:      make(map[int]*trackedProcess),
		recentReaped: make(map[int]recentExit),
	}
}

// StartReaper creates and starts a background Reaper.
func StartReaper(log logr.Logger) *Reaper {
	r := NewReaper(log)
	r.Start()
	return r
}

// Start launches the background reaper goroutine listening for SIGCHLD.
// Safe to call multiple times.
func (r *Reaper) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	r.sigCh = make(chan os.Signal, 128)
	signal.Notify(r.sigCh, syscall.SIGCHLD)
	r.mu.Unlock()

	go r.loop()
}

// Stop terminates the background reaper goroutine, unregisters signal
// notifications, and performs a final non-blocking sweep. Safe to call
// multiple times and safe on a nil Reaper.
func (r *Reaper) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	signal.Stop(r.sigCh)
	close(r.stopCh)
	r.mu.Unlock()

	<-r.doneCh
	r.reap()
}

// SetWaitTimeout overrides the duration WaitResult waits for exit status from
// the background reaper. Non-positive restores defaultReaperWaitTimeout.
func (r *Reaper) SetWaitTimeout(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if d <= 0 {
		r.waitTimeout = defaultReaperWaitTimeout
	} else {
		r.waitTimeout = d
	}
}

// Track registers an active child OS PID so the reaper coordinates with cmd.Wait().
// Safe on a nil Reaper.
func (r *Reaper) Track(pid int) {
	if r == nil || pid <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := newTrackedProcess()
	// Check if the process already exited and was reaped by the background
	// reaper before Track could be invoked (e.g. short-lived fast command).
	if recent, exists := r.recentReaped[pid]; exists {
		entry.set(recent.status)
		delete(r.recentReaped, pid)
		r.reapedOrphans.Add(-1)
	}
	r.tracked[pid] = entry
}

// Untrack unregisters a child OS PID after cmd.Wait() has returned and its
// exit code has been recorded. Safe on a nil Reaper.
func (r *Reaper) Untrack(pid int) {
	if r == nil || pid <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tracked, pid)
	delete(r.recentReaped, pid)
}

// ReapedOrphans returns the total number of orphaned child processes reaped
// by this Reaper that were not actively tracked.
func (r *Reaper) ReapedOrphans() int64 {
	if r == nil {
		return 0
	}
	return r.reapedOrphans.Load()
}

// WaitResult resolves the exit code and error for a command after cmd.Wait().
//
// When cmd.Wait() reaped the process normally, WaitResult maps the exit code
// identically to os/exec. If cmd.Wait() returned ECHILD because this Reaper's
// background wait4(-1, ...) reaped the child first, WaitResult retrieves the
// exit status captured by the reaper, ensuring no exit codes are lost.
//
// Safe on a nil Reaper (falls back to standard cmd.Wait() error interpretation).
func (r *Reaper) WaitResult(cmd *exec.Cmd, waitErr error) (int32, error) {
	if r == nil {
		if waitErr == nil {
			return 0, nil
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			return ExitStatus(exitErr), nil
		}
		return -1, waitErr
	}

	if waitErr == nil {
		return 0, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return ExitStatus(exitErr), nil
	}

	// If cmd.Wait() returned ECHILD, check if this reaper reaped it.
	if errors.Is(waitErr, syscall.ECHILD) && cmd != nil && cmd.Process != nil && cmd.Process.Pid > 0 {
		pid := cmd.Process.Pid
		r.mu.Lock()
		entry, ok := r.tracked[pid]
		timeout := r.waitTimeout
		r.mu.Unlock()

		if ok {
			select {
			case <-entry.done:
				return WaitStatusToExitCode(entry.status), nil
			case <-time.After(timeout):
				r.log.Info("reaper timed out waiting for exit status of tracked process", "pid", pid)
				return -1, waitErr
			}
		}
	}

	return -1, waitErr
}

// Reap performs an immediate non-blocking wait4 sweep of any dead children.
func (r *Reaper) Reap() {
	if r == nil {
		return
	}
	r.reap()
}

func (r *Reaper) loop() {
	defer close(r.doneCh)
	ticker := time.NewTicker(reaperTickInterval)
	defer ticker.Stop()

	// Initial sweep for any processes that died before Start.
	r.reap()

	for {
		select {
		case <-r.stopCh:
			return
		case <-r.sigCh:
			r.reap()
		case <-ticker.C:
			r.reap()
		}
	}
}

func (r *Reaper) reap() {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()

	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if wpid < 0 {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			break
		}
		if wpid == 0 {
			break
		}
		r.handleReaped(wpid, ws)
	}

	// Purge stale recentReaped entries older than 10 seconds.
	r.mu.Lock()
	now := time.Now()
	for pid, exit := range r.recentReaped {
		if now.Sub(exit.timestamp) > 10*time.Second {
			delete(r.recentReaped, pid)
		}
	}
	r.mu.Unlock()
}

func (r *Reaper) handleReaped(pid int, ws syscall.WaitStatus) {
	r.mu.Lock()
	entry, ok := r.tracked[pid]
	if ok {
		entry.set(ws)
	} else {
		r.recentReaped[pid] = recentExit{
			status:    ws,
			timestamp: time.Now(),
		}
		r.reapedOrphans.Add(1)
	}
	r.mu.Unlock()

	if ok {
		r.log.V(4).Info("reaper collected tracked process", "pid", pid, "status", int(ws))
	} else {
		r.log.V(4).Info("reaper reaped orphaned child process", "pid", pid, "status", int(ws))
	}
}

// ExitStatus returns the process's exit code, or 128+n when signal n killed
// it, as a shell reports it.
func ExitStatus(exitErr *exec.ExitError) int32 {
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	return int32(exitErr.ExitCode())
}

// WaitStatusToExitCode converts a syscall.WaitStatus to an exit code,
// returning 128+n when signal n killed it.
func WaitStatusToExitCode(ws syscall.WaitStatus) int32 {
	if ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	return int32(ws.ExitStatus())
}
