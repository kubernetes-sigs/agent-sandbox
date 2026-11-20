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

package runner

import (
	"bufio"
	"io"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

// stderrTailLimit bounds how much child stderr the supervisor retains.
const stderrTailLimit = 4096

// pipeDrainGrace bounds how long the exited child's output pipes are given
// to reach EOF before the read ends are closed to force the consumers back.
const pipeDrainGrace = 5 * time.Second

// secretPattern redacts credential-shaped tokens from captured stderr before
// it is stored or served; the runner must never leak env secrets through the
// status endpoint or logs.
var secretPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]+`)

var defaultReadyPattern = regexp.MustCompile(`(?i)^READY`)

// ProcState is a snapshot of the supervised child process.
type ProcState struct {
	// Alive reports whether the child is currently running.
	Alive bool
	// Ready reports whether the stdout ready-marker has been seen for the
	// current child. For codex exec-server this is configurable; the default
	// pattern matches a line starting with READY.
	Ready bool
	// ExitCode is the last observed exit code, or -1 while alive or when no
	// child has ever been started.
	ExitCode int
	// StderrTail holds up to stderrTailLimit bytes of the child's stderr with
	// credential-shaped tokens redacted.
	StderrTail string
	// StartedAt is when the current/last child was started.
	StartedAt time.Time
}

// Supervisor runs and observes a single long-lived child process (the
// platform executor, e.g. `codex exec-server`). It deliberately has no
// restart policy: crash recovery is the controller's decision, driven by the
// platform re-issuing an environment_connection required action.
type Supervisor struct {
	bin string
	log logr.Logger

	readyPattern *regexp.Regexp

	mu        sync.Mutex
	cmd       *exec.Cmd
	alive     bool
	ready     bool
	exitCode  int
	startedAt time.Time
	stderr    *ringTail
	waitDone  chan struct{} // closed when the wait goroutine records the exit
}

// NewSupervisor returns a Supervisor that launches bin. Nothing is started
// until Start is called.
func NewSupervisor(bin string, log logr.Logger) *Supervisor {
	return &Supervisor{
		bin:          bin,
		log:          log,
		readyPattern: defaultReadyPattern,
		exitCode:     -1,
	}
}

// SetReadyPattern overrides the stdout line pattern that marks the child as
// ready (connected to the platform).
func (s *Supervisor) SetReadyPattern(re *regexp.Regexp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyPattern = re
}

// Start launches the child with the given args and extra env. It fails if a
// child is still alive; starting again after an exit is allowed and resets
// the observed state.
func (s *Supervisor) Start(args []string, env []string) error {
	s.mu.Lock()
	if s.alive {
		s.mu.Unlock()
		return errAlreadyAlive
	}
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(cmd.Environ(), env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return err
	}

	s.cmd = cmd
	s.alive = true
	s.ready = false
	s.exitCode = -1
	s.startedAt = time.Now()
	s.stderr = newRingTail(stderrTailLimit)
	waitDone := make(chan struct{})
	s.waitDone = waitDone
	pattern := s.readyPattern
	ring := s.stderr
	log := s.log
	s.mu.Unlock()

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanReady(stdout, pattern, s, cmd)
	}()
	go ring.consume(stderrPipe)

	go func() {
		defer close(waitDone)
		// Reap the child directly instead of calling cmd.Wait: Wait must not
		// run while the pipe consumers are still reading, and waiting for
		// their EOF first would hang the exit handling when a descendant
		// keeps a pipe open.
		state, err := cmd.Process.Wait()
		code := -1
		if state != nil {
			code = state.ExitCode()
		}
		s.mu.Lock()
		s.alive = false
		s.exitCode = code
		s.mu.Unlock()
		if err != nil && code > 0 {
			// Expected for crash modes; the tail is served via State().
			log.V(2).Info("executor exited", "exitCode", code)
		}
		// Bounded drain: normally the write ends died with the child, EOF is
		// immediate, and the crash tail is complete. A descendant holding a
		// pipe open gets the read ends closed on it instead.
		drainDone := make(chan struct{})
		go func() {
			<-stdoutDone
			<-ring.done
			close(drainDone)
		}()
		select {
		case <-drainDone:
		case <-time.After(pipeDrainGrace):
			_ = stdout.Close()
			_ = stderrPipe.Close()
			<-drainDone
		}
		// cmd.Wait never runs, so close the descriptors the consumers
		// finished with.
		_ = stdout.Close()
		_ = stderrPipe.Close()
	}()
	return nil
}

// Stop terminates the child: SIGTERM, then SIGKILL after grace. forced
// reports whether SIGKILL was needed. Stopping a non-alive child is a no-op.
func (s *Supervisor) Stop(grace time.Duration) (forced bool) {
	s.mu.Lock()
	if !s.alive || s.cmd == nil || s.cmd.Process == nil {
		s.mu.Unlock()
		return false
	}
	proc := s.cmd.Process
	waitDone := s.waitDone
	s.mu.Unlock()

	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-waitDone:
		return false
	case <-time.After(grace):
	}
	_ = proc.Signal(syscall.SIGKILL)
	<-waitDone
	return true
}

// State returns a snapshot of the supervised child.
func (s *Supervisor) State() ProcState {
	s.mu.Lock()
	ring := s.stderr
	st := ProcState{
		Alive:     s.alive,
		Ready:     s.ready,
		ExitCode:  s.exitCode,
		StartedAt: s.startedAt,
	}
	s.mu.Unlock()
	if ring != nil {
		st.StderrTail = ring.snapshotRedacted()
	}
	return st
}

// markReady records the ready marker only when it came from the current
// child: a late-scanned line from a previous process must not mark its
// replacement ready.
func (s *Supervisor) markReady(cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != cmd {
		return
	}
	s.ready = true
}

// errAlreadyAlive is returned by Start when a child is still running.
var errAlreadyAlive = &StartError{Reason: "executor already running"}

// StartError is returned for definite start failures so callers can classify
// them without string matching.
type StartError struct{ Reason string }

func (e *StartError) Error() string { return "supervisor: " + e.Reason }

// scanReady watches the child's stdout for the ready marker and keeps
// draining to EOF afterwards: returning early would let the child block on
// a full stdout pipe once it logs past 64KiB.
func scanReady(r io.Reader, pattern *regexp.Regexp, s *Supervisor, cmd *exec.Cmd) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	marked := false
	for sc.Scan() {
		if !marked && pattern.MatchString(sc.Text()) {
			s.markReady(cmd)
			marked = true
		}
	}
}

// ringTail keeps the last limit bytes written to it.
type ringTail struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	done  chan struct{}
}

func newRingTail(limit int) *ringTail {
	return &ringTail{limit: limit, done: make(chan struct{})}
}

func (r *ringTail) consume(src io.Reader) {
	defer close(r.done)
	p := make([]byte, 512)
	for {
		n, err := src.Read(p)
		if n > 0 {
			r.append(p[:n])
		}
		if err != nil {
			return
		}
	}
}

// secretTokenByte matches the body class of secretPattern. Eviction must not
// cut inside a run of these bytes: the orphaned suffix of a credential has
// no "sk-" prefix left, so snapshotRedacted could not recognize it.
func secretTokenByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// maxEvictSkip bounds how far eviction advances to a token boundary. Runs
// longer than this are data (e.g. base64 blobs), not credentials, and are
// kept rather than discarded wholesale.
const maxEvictSkip = 512

func (r *ringTail) append(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, b...)
	if len(r.buf) > r.limit {
		cut := len(r.buf) - r.limit
		end := cut
		for end < len(r.buf) && secretTokenByte(r.buf[end]) {
			end++
		}
		if end-cut <= maxEvictSkip {
			// Drop the token run the cut landed in instead of keeping its
			// unredactable tail.
			cut = end
		}
		r.buf = r.buf[cut:]
	}
}

// snapshotRedacted returns the retained tail with secret-shaped tokens
// replaced. It waits briefly for the consumer to finish so an exited child's
// full tail is visible.
func (r *ringTail) snapshotRedacted() string {
	select {
	case <-r.done:
	case <-time.After(50 * time.Millisecond):
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buf == nil {
		return ""
	}
	return secretPattern.ReplaceAllString(string(r.buf), "[redacted]")
}
