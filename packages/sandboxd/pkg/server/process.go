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

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// mapCommandError converts command execution errors (e.g. missing binary or
// permission denied) into appropriate gRPC status codes (NOT_FOUND,
// PERMISSION_DENIED) instead of generic INTERNAL error codes.
func mapCommandError(err error, msg string) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return status.Errorf(codes.NotFound, "%s: command or path not found: %v", msg, err)
	}
	if errors.Is(err, fs.ErrPermission) {
		return status.Errorf(codes.PermissionDenied, "%s: permission denied: %v", msg, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", msg, err)
}

// defaultPipeCapacity is the Linux pipe capacity. It bounds the output still
// delivered after the child exits when the queued byte count is unavailable.
const defaultPipeCapacity = 64 * 1024

// ptyQueueBound bounds the output a PTY can still hold when the child exits.
// The queued byte count cannot be read for a PTY: TIOCINQ on the master
// counts only the line discipline's 4 KiB read buffer, not the flip buffer
// behind it. Linux limits a PTY's flip buffer to 8 KiB of buffers but can
// overshoot by one buffer, so a PTY holds 10 to 19 KiB in all on 4 KiB pages,
// depending on the write size. The bound leaves room for larger pages.
const ptyQueueBound = 256 * 1024

// defaultStreamChunkSize is the default read buffer size for stdout/stderr
// streaming, overridable via the --stream-chunk-size daemon flag. Timing
// constants (pipeDrainGrace, ...) are collected in one block in server.go.
const defaultStreamChunkSize = 4096

// ProcessServer implements the ProcessService gRPC API defined in
// packages/sandboxd/spec/process/v1/process.proto.
type ProcessServer struct {
	processv1.UnimplementedProcessServiceServer
	rootDir         string
	registry        *processmanager.ProcessRegistry
	streamChunkSize int
	executionPolicy ExecutionPolicy
	log             logr.Logger
}

// NewProcessServer builds a ProcessServer rooted at rootDir. rootDir must be
// non-empty. A nil registry gets a fresh one, but callers normally share the
// daemon-wide registry so shutdown can signal every child. A non-positive
// streamChunkSize selects the default.
func NewProcessServer(rootDir string, registry *processmanager.ProcessRegistry, streamChunkSize int, log logr.Logger) *ProcessServer {
	if registry == nil {
		registry = processmanager.NewProcessRegistry()
	}
	if streamChunkSize <= 0 {
		streamChunkSize = defaultStreamChunkSize
	}
	// Default a nil/zero-value logger to Discard so that s.log.Error
	// calls in killProcessGroupOnContextDone never panic.
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &ProcessServer{
		rootDir:         rootDir,
		registry:        registry,
		streamChunkSize: streamChunkSize,
		log:             log,
	}
}

// buildCommand translates a ProcessConfig into an exec.Cmd with env merged
// over the daemon environment and cwd confined to rootDir.
//
// Unlike exec.CommandContext, this does NOT install Go's default context
// cancellation handler (which only sends SIGKILL to the child PID). Callers
// are responsible for killing the process group on context cancellation —
// see killProcessGroupOnContextDone — so that grandchildren are reaped too.
func (s *ProcessServer) buildCommand(_ context.Context, config *processv1.ProcessConfig) (*exec.Cmd, error) {
	if config == nil || len(config.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	command := config.GetCommand()
	cmd := exec.Command(command[0], command[1:]...)

	// A nil cmd.Env inherits the daemon environment automatically, so it is
	// only materialized when the request adds vars. Request vars are
	// appended AFTER os.Environ() deliberately: os/exec resolves duplicate
	// keys to the last value, so request-supplied vars override daemon ones.
	if len(config.GetEnvVars()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range config.GetEnvVars() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if config.GetCwd() != "" {
		// Security check: confine the requested working directory to the
		// sandbox root with the same symlink-aware sanitization the
		// filesystem API uses; escaping paths get PERMISSION_DENIED.
		sanitizedCwd, err := pathutil.SanitizePath(s.rootDir, config.GetCwd())
		if err != nil {
			if errors.Is(err, pathutil.ErrPathEscapes) {
				return nil, status.Errorf(codes.PermissionDenied, "cwd: %v", err)
			}
			if errors.Is(err, syscall.ENOTDIR) {
				return nil, status.Errorf(codes.NotFound, "cwd: path does not exist: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "cwd: %v", err)
		}
		// Match WatchDir: a path that exists but is not a directory is an
		// invalid argument. A missing leaf is NOT_FOUND. ENOTDIR from
		// executing a command path such as "f.txt/prog" stays in
		// mapCommandError, where NOT_FOUND is the right code.
		info, err := os.Stat(sanitizedCwd)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, status.Errorf(codes.NotFound, "cwd: path does not exist: %s", config.GetCwd())
			}
			return nil, status.Errorf(codes.Internal, "cwd: %v", err)
		}
		if !info.IsDir() {
			return nil, status.Errorf(codes.InvalidArgument, "cwd is not a directory: %s", config.GetCwd())
		}
		cmd.Dir = sanitizedCwd
	} else {
		cmd.Dir = s.rootDir
	}

	return cmd, nil
}

// Start runs a command and streams stdout/stderr events until it exits.
func (s *ProcessServer) Start(req *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
	cmd, err := s.buildCommand(stream.Context(), req.GetConfig())
	if err != nil {
		return err
	}

	pid := s.registry.NextPID()
	proc := &processmanager.ManagedProcess{
		ID:   pid,
		Cmd:  cmd,
		Done: make(chan struct{}),
	}

	cmd.WaitDelay = waitDelay

	var ptyFile *os.File
	var stdoutR, stdoutW, stderrR, stderrW, stdinR, stdinW *os.File

	usePTY := req.GetPty() != nil
	if usePTY {
		// Bail out early if the client already disconnected — no point
		// forking a process we would immediately have to kill.
		if err := stream.Context().Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		// creack/pty sets Setsid on the child, which already places it in
		// its own process group (pgid == pid), so process-group signalling
		// works without Setpgid. Setting both would make fork fail with
		// EPERM (setpgid is illegal on a session leader).
		//
		// The initial size must be applied before the child starts: setting
		// it afterwards races with a child that reads the terminal size
		// immediately (e.g. `stty size` would report 0 0).
		if req.GetPty().GetCols() > 0 && req.GetPty().GetRows() > 0 {
			ptyFile, err = pty.StartWithSize(cmd, &pty.Winsize{
				Cols: uint16(req.GetPty().GetCols()),
				Rows: uint16(req.GetPty().GetRows()),
			})
		} else {
			ptyFile, err = pty.Start(cmd)
		}
		if err != nil {
			return mapCommandError(err, "failed to start command with PTY")
		}
		proc.PTY = ptyFile
		proc.Stdin = ptyFile
	} else {
		// Put the child in its own process group so SendSignal reaches the
		// whole tree and shutdown sweeps don't leak grandchildren.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdoutR, stdoutW, err = os.Pipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to create stdout pipe: %v", err)
		}
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			closeAll(stdoutR, stdoutW)
			return status.Errorf(codes.Internal, "failed to create stderr pipe: %v", err)
		}
		stdinR, stdinW, err = os.Pipe()
		if err != nil {
			closeAll(stdoutR, stdoutW, stderrR, stderrW)
			return status.Errorf(codes.Internal, "failed to create stdin pipe: %v", err)
		}

		cmd.Stdout, cmd.Stderr, cmd.Stdin = stdoutW, stderrW, stdinR
		proc.Stdin = stdinW

		// Bail out early if the client already disconnected.
		if err := stream.Context().Err(); err != nil {
			closeAll(stdoutR, stdoutW, stderrR, stderrW, stdinR, stdinW)
			return status.FromContextError(err).Err()
		}

		if err := cmd.Start(); err != nil {
			closeAll(stdoutR, stdoutW, stderrR, stderrW, stdinR, stdinW)
			return mapCommandError(err, "failed to start command")
		}
		// Process owns its write end copies; close ours so readers get EOF.
		closeAll(stdoutW, stderrW, stdinR)
	}

	// Register BEFORE sending InitEvent so a client that calls WriteStdin /
	// SendSignal immediately after receiving the event never races the
	// registry.
	s.registry.Register(proc)
	defer s.registry.Remove(pid)

	// Kill the entire process group (not just the child PID) when the
	// stream context is cancelled (client disconnects). This prevents
	// grandchildren from becoming orphan processes.
	stopCancelKill := s.killProcessGroupOnContextDone(stream.Context(), cmd)

	if err := stream.Send(&processv1.StartResponse{
		Event: &processv1.StartResponse_Init{
			Init: &processv1.InitEvent{ProcessId: pid},
		},
	}); err != nil {
		_ = proc.Signal(syscall.SIGKILL)
		closeAll(stdoutR, stderrR, stdinW)
		go func() {
			stopCancelKill()
			_ = cmd.Wait()
			_ = proc.ClosePTY()
			close(proc.Done)
		}()
		return status.Errorf(codes.Internal, "failed to send InitEvent: %v", err)
	}

	var streamWg sync.WaitGroup
	var sendMu sync.Mutex
	sends := newSendTracker()

	// streamOutput copies out.file into stdout/stderr stream events until the
	// file is exhausted (EOF on pipes, EIO/ErrClosed on a PTY).
	streamOutput := func(out *trackedOutput, isStderr bool) {
		defer streamWg.Done()
		buf := make([]byte, s.streamChunkSize)
		for {
			n, rErr := out.file.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				var event *processv1.StartResponse
				if isStderr {
					event = &processv1.StartResponse{Event: &processv1.StartResponse_Stderr{Stderr: chunk}}
				} else {
					event = &processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: chunk}}
				}
				counted := sends.begin(out, n)
				sendMu.Lock()
				sErr := stream.Send(event)
				sendMu.Unlock()
				sends.end(counted)
				if sErr != nil {
					return
				}
			}
			if rErr != nil {
				return
			}
		}
	}

	var waitErr error
	if usePTY {
		streamWg.Add(1)
		go streamOutput(sends.track(ptyFile, ptyQueueBound), false)
		// For a PTY the reader unblocks with EIO once the child exits and
		// the kernel's buffered output is drained. Closing the master right
		// after Wait would discard that buffer, dropping the output of a
		// short-lived process, so give the reader a chance to finish first.
		// Closing the PTY only on timeout unblocks a straggling read (e.g. a
		// grandchild still holds the slave side).
		waitForExit(cmd.Process.Pid)
		sends.childExited(s.streamChunkSize)
		waitForReaders(&streamWg, sends, time.After, func() { _ = proc.ClosePTY() })
		_ = proc.ClosePTY()
		stopCancelKill()
		waitErr = cmd.Wait()
	} else {
		streamWg.Add(2)
		go streamOutput(sends.track(stdoutR, -1), false)
		go streamOutput(sends.track(stderrR, -1), true)

		waitForExit(cmd.Process.Pid)
		sends.childExited(s.streamChunkSize)
		waitForReaders(&streamWg, sends, time.After, func() { closeAll(stdoutR, stderrR) })
		closeAll(stdoutR, stderrR)
		stopCancelKill()
		waitErr = cmd.Wait()
	}

	exitCode := int32(0)
	if waitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			exitCode = exitStatus(exitErr)
		} else {
			exitCode = -1
		}
	}

	proc.SetExitCode(exitCode)
	closeAll(stdinW) // close stdin pipe; safe no-op if nil (PTY path)
	close(proc.Done)

	if err := stream.Send(&processv1.StartResponse{
		Event: &processv1.StartResponse_Exit{
			Exit: &processv1.ExitEvent{ExitCode: exitCode},
		},
	}); err != nil {
		return status.Errorf(codes.Internal, "failed to send ExitEvent: %v", err)
	}
	return nil
}

// Execute runs a command synchronously and returns its buffered output.
func (s *ProcessServer) Execute(ctx context.Context, req *processv1.ExecuteRequest) (*processv1.ExecuteResponse, error) {
	limits, err := s.executionPolicy.resolve(nil)
	if err != nil {
		return nil, err
	}
	result, err := s.execute(ctx, req.GetConfig(), limits)
	if err != nil {
		return nil, err
	}
	if result.TerminationReason == processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED {
		return nil, status.Error(codes.ResourceExhausted, "Execute exceeded its execution time limit; ExecuteWithLimits reports structured outcomes")
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return nil, status.Error(codes.ResourceExhausted, "Execute output was truncated by deployment policy; ExecuteWithLimits reports retained output")
	}
	return &processv1.ExecuteResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, nil
}

// ExecuteWithLimits retains bounded output without interrupting pipe draining.
func (s *ProcessServer) ExecuteWithLimits(ctx context.Context, req *processv1.ExecuteWithLimitsRequest) (*processv1.ExecuteWithLimitsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	limits, err := s.executionPolicy.resolve(req)
	if err != nil {
		return nil, err
	}
	return s.execute(ctx, req.GetConfig(), limits)
}

type executionLimits struct {
	timeLimit                time.Duration
	stdoutBytes, stderrBytes uint64
}

func requestedTimeLimit(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "invalid time_limit: %v", err)
	}
	v := d.AsDuration()
	// AsDuration saturates on overflow; a requested bound must remain exact.
	roundTrip := durationpb.New(v)
	if v <= 0 || roundTrip.Seconds != d.Seconds || roundTrip.Nanos != d.Nanos {
		return 0, status.Error(codes.InvalidArgument, "time_limit must be positive and fit in a Go duration")
	}
	return v, nil
}

// executionStream accepts the entire stream while retaining only its prefix.
// A zero internal limit preserves the unconfigured, unlimited behavior.
type executionStream struct {
	buffer         bytes.Buffer
	limit          uint64
	truncated      bool
	reader, writer *os.File
	done           chan struct{}
	err            error
}

func (b *executionStream) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit != 0 {
		remaining := b.limit - uint64(b.buffer.Len())
		if uint64(n) > remaining {
			p = p[:int(remaining)]
			b.truncated = true
		}
	}
	_, err := b.buffer.Write(p)
	return n, err
}

func newExecutionStream(limit uint64) (*executionStream, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &executionStream{limit: limit, reader: r, writer: w, done: make(chan struct{})}, nil
}

func (b *executionStream) close() {
	_ = b.reader.Close()
	_ = b.writer.Close()
}

func (b *executionStream) collect() {
	_ = b.writer.Close()
	go func() {
		_, b.err = io.Copy(b, b.reader)
		_ = b.reader.Close()
		close(b.done)
	}()
}

type unaryExecution struct {
	cmd            *exec.Cmd
	stdout, stderr *executionStream
	drainGrace     time.Duration
}

type executionOutcome struct {
	exitCode          int32
	timeLimitExceeded bool
}

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (run unaryExecution) finished(exited <-chan struct{}) bool {
	return channelClosed(exited) && channelClosed(run.stdout.done) && channelClosed(run.stderr.done)
}

func (s *ProcessServer) execute(ctx context.Context, config *processv1.ProcessConfig, limits executionLimits) (*processv1.ExecuteWithLimitsResponse, error) {
	cmd, err := s.buildCommand(ctx, config)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	drainGrace := waitDelay
	if limits.timeLimit > 0 {
		drainGrace = pipeDrainGrace
	}

	stdoutBuf, err := newExecutionStream(limits.stdoutBytes)
	if err != nil {
		return nil, mapCommandError(err, "create stdout pipe")
	}
	defer stdoutBuf.close()
	stderrBuf, err := newExecutionStream(limits.stderrBytes)
	if err != nil {
		return nil, mapCommandError(err, "create stderr pipe")
	}
	defer stderrBuf.close()
	cmd.Stdout = stdoutBuf.writer
	cmd.Stderr = stderrBuf.writer

	// Bail out early if the caller's context is already cancelled.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	started := time.Now()
	var timeExpired <-chan time.Time
	if limits.timeLimit > 0 {
		timer := time.NewTimer(limits.timeLimit)
		defer timer.Stop()
		timeExpired = timer.C
	}
	if err := cmd.Start(); err != nil {
		return nil, mapCommandError(err, "failed to execute command")
	}
	stdoutBuf.collect()
	stderrBuf.collect()
	defer func() {
		// Closing readers also unblocks collection on a cleanup-error path.
		stdoutBuf.close()
		stderrBuf.close()
		<-stdoutBuf.done
		<-stderrBuf.done
	}()

	outcome, err := s.waitExecution(ctx, unaryExecution{cmd, stdoutBuf, stderrBuf, drainGrace}, timeExpired)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	reason := processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED
	if outcome.timeLimitExceeded {
		reason = processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED
	}

	return &processv1.ExecuteWithLimitsResponse{
		ExitCode:          outcome.exitCode,
		Stdout:            stdoutBuf.buffer.Bytes(),
		Stderr:            stderrBuf.buffer.Bytes(),
		TerminationReason: reason,
		ElapsedDuration:   durationpb.New(time.Since(started)),
		StdoutTruncated:   stdoutBuf.truncated,
		StderrTruncated:   stderrBuf.truncated,
	}, nil
}

// waitExecution owns the sole Wait call and joins termination before inspecting
// output. Even if cleanup times out, the Wait goroutine still reaps the child.
//
// The leader stays unreaped until the loop ends, so its PID cannot be reused
// while termination may still signal it or its process group.
func (s *ProcessServer) waitExecution(ctx context.Context, run unaryExecution, timeExpired <-chan time.Time) (executionOutcome, error) {
	exited := make(chan struct{})
	release := make(chan struct{})
	waitDone := make(chan struct{})
	var runErr error
	go func() {
		waitForExit(run.cmd.Process.Pid)
		close(exited)
		<-release
		runErr = run.cmd.Wait()
		close(waitDone)
	}()
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	var pendingKill chan struct{}
	defer func() {
		// On an early return the leader stays unreaped until termination ends.
		if pendingKill == nil {
			releaseLeader()
			return
		}
		go func() {
			<-pendingKill
			releaseLeader()
		}()
	}()
	var outcome executionOutcome
	pendingWait, pendingOut, pendingErr := exited, run.stdout.done, run.stderr.done
	ctxDone := ctx.Done()
	var cleanupDone <-chan struct{}
	var cleanupCancel context.CancelFunc
	var drainDone <-chan time.Time
	var drainTimer *time.Timer
	var drainErr error
	startCleanup := func() {
		ctxDone, timeExpired = nil, nil
		if cleanupCancel != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), executionCleanupTimeout)
		cleanupCancel = cancel
		cleanupDone = cleanupCtx.Done()
		pendingKill = s.terminateExecution(cleanupCtx, run.cmd)
	}
	defer func() {
		if cleanupCancel != nil {
			cleanupCancel()
		}
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	for pendingWait != nil || pendingOut != nil || pendingErr != nil || pendingKill != nil {
		select {
		case <-pendingWait:
			pendingWait = nil
			if pendingOut != nil || pendingErr != nil {
				drainTimer = time.NewTimer(run.drainGrace)
				drainDone = drainTimer.C
			}
		case <-pendingOut:
			pendingOut = nil
		case <-pendingErr:
			pendingErr = nil
		case <-pendingKill:
			pendingKill = nil
			if drainErr != nil {
				run.stdout.close()
				run.stderr.close()
			}
		case <-ctxDone:
			ctxDone, timeExpired = nil, nil
			if run.finished(exited) {
				continue
			}
			startCleanup()
		case <-timeExpired:
			ctxDone, timeExpired = nil, nil
			if run.finished(exited) {
				continue
			}
			outcome.timeLimitExceeded = true
			startCleanup()
		case <-cleanupDone:
			if run.finished(exited) && (pendingKill == nil || channelClosed(pendingKill)) {
				cleanupDone = nil
				continue
			}
			if err := ctx.Err(); err != nil {
				return executionOutcome{}, status.FromContextError(err).Err()
			}
			return executionOutcome{}, status.Error(codes.Internal, "command cleanup exceeded its waiting limit")
		case <-drainDone:
			if channelClosed(run.stdout.done) && channelClosed(run.stderr.done) {
				drainDone = nil
				continue
			}
			// A failed drain still owns the group. Finish its termination
			// attempt before closing readers and returning the drain error.
			drainDone = nil
			drainErr = status.Error(codes.Internal, "command output pipes did not finish draining")
			startCleanup()
			if pendingKill == nil {
				run.stdout.close()
				run.stderr.close()
			}
		}
		if pendingOut == nil && pendingErr == nil && drainTimer != nil {
			drainTimer.Stop()
			drainDone = nil
		}
	}
	releaseLeader()
	<-waitDone
	if err := ctx.Err(); err != nil {
		return executionOutcome{}, status.FromContextError(err).Err()
	}
	if drainErr != nil {
		return executionOutcome{}, drainErr
	}
	if run.stdout.err != nil {
		return executionOutcome{}, status.Errorf(codes.Internal, "failed to collect stdout: %v", run.stdout.err)
	}
	if run.stderr.err != nil {
		return executionOutcome{}, status.Errorf(codes.Internal, "failed to collect stderr: %v", run.stderr.err)
	}
	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			outcome.exitCode = exitStatus(exitErr)
		} else {
			return executionOutcome{}, mapCommandError(runErr, "failed to execute command")
		}
	}
	return outcome, nil
}

// waitForExit blocks until the process pid has exited, without reaping it. An
// unreaped process keeps its PID reserved, so signalling the PID or its process
// group stays safe until the caller reaps it with cmd.Wait.
func waitForExit(pid int) {
	var info [128]byte // siginfo_t
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno != syscall.EINTR {
			return
		}
	}
}

func (s *ProcessServer) terminateExecution(ctx context.Context, cmd *exec.Cmd) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.killProcessTree(ctx, cmd)
	}()
	return done
}

// WriteStdin sends input bytes or EOF to a running process.
func (s *ProcessServer) WriteStdin(_ context.Context, req *processv1.WriteStdinRequest) (*processv1.WriteStdinResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %d not found", req.GetProcessId())
	}

	if req.GetEof() != nil {
		if err := proc.CloseStdin(); err != nil {
			if errors.Is(err, processmanager.ErrStdinIsTerminal) {
				return nil, status.Errorf(codes.FailedPrecondition,
					"process %d has a PTY; write 0x04 (Ctrl-D) at the start of a line to end input, or use SendSignal", req.GetProcessId())
			}
			return nil, status.Errorf(codes.Internal, "failed to close stdin: %v", err)
		}
		return &processv1.WriteStdinResponse{}, nil
	}

	if proc.Stdin == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "process %d has no stdin", req.GetProcessId())
	}
	if input := req.GetInput(); len(input) > 0 {
		if _, err := proc.Stdin.Write(input); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to write stdin: %v", err)
		}
	}

	return &processv1.WriteStdinResponse{}, nil
}

// SendSignal delivers a POSIX signal to a running process group.
func (s *ProcessServer) SendSignal(_ context.Context, req *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %d not found", req.GetProcessId())
	}

	var sig syscall.Signal
	switch req.GetSignal() {
	case processv1.Signal_SIGNAL_SIGINT:
		sig = syscall.SIGINT
	case processv1.Signal_SIGNAL_SIGTERM:
		sig = syscall.SIGTERM
	case processv1.Signal_SIGNAL_SIGKILL:
		sig = syscall.SIGKILL
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported or unspecified signal: %v", req.GetSignal())
	}

	if err := proc.Signal(sig); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send signal: %v", err)
	}

	return &processv1.SendSignalResponse{}, nil
}

// ResizeTTY resizes the pseudo-terminal window of a running PTY process.
func (s *ProcessServer) ResizeTTY(_ context.Context, req *processv1.ResizeTTYRequest) (*processv1.ResizeTTYResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %d not found", req.GetProcessId())
	}

	if err := proc.ResizeTTY(uint16(req.GetCols()), uint16(req.GetRows())); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "failed to resize TTY: %v", err)
	}

	return &processv1.ResizeTTYResponse{}, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// killProcessGroupOnContextDone starts a goroutine that terminates the
// command's entire process tree when ctx is cancelled. This replaces the
// default exec.CommandContext behaviour (which only sends SIGKILL to the
// child PID) so that grandchildren are reaped as well.
//
// The returned stop function prevents any new termination and waits for one
// in progress. Callers must call it before cmd.Wait reaps the leader, since
// its PID/PGID may be reused by an unrelated process afterwards.
//
// The kill strategy is layered to work across container runtimes:
//
//  1. syscall.Kill(-PGID, SIGKILL) — signals the whole process group in one
//     shot. Works on real Linux where Setpgid/Setsid places the child and
//     its descendants in the same process group.
//  2. /proc descendant scan — walks /proc to find children of the command
//     PID and kills each recursively. This is the fallback for runtimes
//     like gVisor (runsc) where process-group signalling does not reliably
//     propagate to grandchildren.
//  3. Direct PID kill — last-resort kill of the child PID itself, in case
//     both of the above failed to reach it.
//
// The caller must have set Setpgid (or Setsid via PTY) on the command so
// that the child and its descendants share a process group.
func (s *ProcessServer) killProcessGroupOnContextDone(ctx context.Context, cmd *exec.Cmd) (stop func()) {
	var mu sync.Mutex
	var stopped bool
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case <-ctx.Done():
			mu.Lock()
			defer mu.Unlock()
			if !stopped {
				s.killProcessTree(context.Background(), cmd)
			}
		case <-stopCh:
		}
	}()
	return func() {
		stopOnce.Do(func() {
			// Taking the lock waits for a termination already in progress.
			mu.Lock()
			stopped = true
			mu.Unlock()
			close(stopCh)
		})
	}
}

func (s *ProcessServer) killProcessTree(ctx context.Context, cmd *exec.Cmd) {
	// The group can outlive its leader and keep output pipes open. Reaping
	// the leader must not suppress termination of the remaining group.
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.log.Error(err, "failed to kill process group", "pgid", pid)
	}
	s.killDescendants(ctx, pid)
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		s.log.Error(err, "failed to kill child process", "pid", pid)
	}
}

// killDescendants recursively kills all descendant processes of rootPID by
// walking /proc. On real Linux, killProcessGroupOnContextDone's strategy 1
// already killed the tree, so the Kill calls below return ESRCH and are
// silently skipped. In runtimes like gVisor where PGID kill does not
// propagate, this ensures grandchildren do not outlive the parent.
func (s *ProcessServer) killDescendants(ctx context.Context, rootPID int) {
	for _, cpid := range readProcChildren(ctx, rootPID) {
		if ctx.Err() != nil {
			return
		}
		// Recurse first so we kill leaves before their parents — otherwise
		// a child reaped mid-walk could reparent its children to init
		// before we get a chance to signal them.
		s.killDescendants(ctx, cpid)
		if err := syscall.Kill(cpid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			s.log.V(1).Info("failed to kill descendant process", "pid", cpid, "error", err)
		}
	}
}

// readProcChildren returns the direct children of pid by iterating every
// thread's /proc/<pid>/task/<tid>/children (a Linux-specific interface
// enabled by CONFIG_PROC_CHILDREN). Children forked by a non-main thread
// only appear under that thread's TID, so scanning only the main PID
// would miss grandchildren of multithreaded parents. Returns nil when the
// directory is unavailable.
func readProcChildren(ctx context.Context, pid int) []int {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{})
	var children []int
	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%s/children", taskDir, entry.Name()))
		if err != nil {
			continue
		}
		for f := range strings.FieldsSeq(string(data)) {
			if cpid, err := strconv.Atoi(f); err == nil && cpid > 0 {
				if _, ok := seen[cpid]; !ok {
					seen[cpid] = struct{}{}
					children = append(children, cpid)
				}
			}
		}
	}
	return children
}

// sendTracker records how many output readers are delivering output that was
// written before the child exited, including waiting for their turn on the
// stream. waitForReaders does not count that time against pipeDrainGrace.
// Output written after the exit, by a descendant that still holds the pipe,
// does not pause the grace, so such a descendant cannot keep the stream open
// by writing to a slow client.
type sendTracker struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
	outputs []*trackedOutput
}

// trackedOutput is one output reader's position in its pipe or PTY. read and
// limit are guarded by sendTracker.mu.
type trackedOutput struct {
	file *os.File
	// queueBound bounds the bytes file can hold, or is -1 to ask the
	// kernel how many bytes it holds.
	queueBound int64
	// read counts the bytes read from file so far.
	read int64
	// limit is the read offset past which output was written after the
	// child exited, or -1 while the child runs.
	limit int64
}

func newSendTracker() *sendTracker {
	return &sendTracker{changed: make(chan struct{})}
}

// track registers an output reader. Call it before the reader starts.
// queueBound bounds the bytes file can hold; pass -1 for a pipe, whose queued
// byte count the kernel reports.
func (t *sendTracker) track(file *os.File, queueBound int64) *trackedOutput {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := &trackedOutput{file: file, queueBound: queueBound, limit: -1}
	t.outputs = append(t.outputs, out)
	return out
}

// childExited sets each reader's limit to the output it has read plus what
// is still queued in its pipe or PTY, or the bound on that. A read in progress
// has already taken its bytes from the queue but not yet counted them, so the
// limit allows one extra chunk.
func (t *sendTracker) childExited(chunk int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, out := range t.outputs {
		queued := out.queueBound
		if queued < 0 {
			queued = queuedOutputBytes(out.file)
		}
		out.limit = out.read + queued + int64(chunk)
	}
}

// begin records that out read n bytes and is about to send them. It reports
// whether the send pauses the grace, which callers pass to end.
func (t *sendTracker) begin(out *trackedOutput, n int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := out.read
	out.read += int64(n)
	if out.limit >= 0 && start >= out.limit {
		return false
	}
	t.active++
	t.notifyLocked()
	return true
}

// end records that a send started by begin finished.
func (t *sendTracker) end(counted bool) {
	if !counted {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	t.notifyLocked()
}

func (t *sendTracker) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// expire reports whether the grace may end: no reader is sending output
// written before the child exited. It takes mu, as begin does, so a send that
// begin counted before the check holds the grace open, and one counted after
// it started after the grace ended.
func (t *sendTracker) expire() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active == 0
}

// state reports whether any reader is sending output written before the
// child exited, and returns a channel that is closed at the next change.
func (t *sendTracker) state() (bool, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active > 0, t.changed
}

// waitForReaders blocks until every output reader in wg has finished. Readers
// get pipeDrainGrace to reach EOF on their own; after that forceClose is called
// to unblock any that are stuck (a grandchild may still hold the write side),
// and waitForReaders waits for them to return.
//
// The grace only runs while no reader is sending output that was written
// before the child exited. A slow client blocks the readers in stream.Send
// while the pipe still holds that output, and closing the pipe then would
// discard it. Output written after the exit does not pause the grace, so a
// descendant that keeps writing ends the stream as before. A reader that is
// sending cannot be stopped by forceClose anyway, so pausing the grace does
// not let a stalled client hold the stream open any longer than it already
// could. after starts the grace timer; Start passes time.After.
func waitForReaders(wg *sync.WaitGroup, sends *sendTracker, after func(time.Duration) <-chan time.Time, forceClose func()) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	remaining := pipeDrainGrace
	for {
		sending, changed := sends.state()
		if sending {
			select {
			case <-done:
				return
			case <-changed:
				continue
			}
		}
		started := time.Now()
		select {
		case <-done:
			return
		case <-after(remaining):
			// A reader may have begun a counted send since state, and
			// select picks at random when changed is ready too. Wait for
			// that send before closing the readers.
			if !sends.expire() {
				remaining = 0
				continue
			}
			forceClose()
			<-done
			return
		case <-changed:
			remaining = max(0, remaining-time.Since(started))
		}
	}
}

// exitStatus returns the process's exit code, or 128+n when signal n killed
// it, as a shell reports it. exec.ExitError.ExitCode returns -1 for a
// signal, which is indistinguishable from sandboxd's own failure value.
func exitStatus(exitErr *exec.ExitError) int32 {
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	return int32(exitErr.ExitCode())
}
