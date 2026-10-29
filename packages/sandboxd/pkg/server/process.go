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
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return status.Errorf(codes.NotFound, "%s: command or path not found: %v", msg, err)
	}
	if errors.Is(err, fs.ErrPermission) {
		return status.Errorf(codes.PermissionDenied, "%s: permission denied: %v", msg, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", msg, err)
}

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
			return nil, status.Errorf(codes.Internal, "cwd: %v", err)
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
	s.killProcessGroupOnContextDone(stream.Context(), cmd, proc.Done)

	if err := stream.Send(&processv1.StartResponse{
		Event: &processv1.StartResponse_Init{
			Init: &processv1.InitEvent{ProcessId: pid},
		},
	}); err != nil {
		_ = proc.Signal(syscall.SIGKILL)
		closeAll(stdoutR, stderrR, stdinW)
		go func() {
			_ = cmd.Wait()
			_ = proc.ClosePTY()
			close(proc.Done) // release the cancellation watcher so it doesn't leak
		}()
		return status.Errorf(codes.Internal, "failed to send InitEvent: %v", err)
	}

	var streamWg sync.WaitGroup
	var sendMu sync.Mutex

	// streamOutput copies reader into stdout/stderr stream events until the
	// reader is exhausted (EOF on pipes, EIO/ErrClosed on a PTY).
	streamOutput := func(reader io.Reader, isStderr bool) {
		defer streamWg.Done()
		buf := make([]byte, s.streamChunkSize)
		for {
			n, rErr := reader.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				var event *processv1.StartResponse
				if isStderr {
					event = &processv1.StartResponse{Event: &processv1.StartResponse_Stderr{Stderr: chunk}}
				} else {
					event = &processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: chunk}}
				}
				sendMu.Lock()
				sErr := stream.Send(event)
				sendMu.Unlock()
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
		go streamOutput(ptyFile, false)
		// For a PTY the reader unblocks with EIO once the child exits and
		// the kernel's buffered output is drained. Closing the master right
		// after Wait would discard that buffer, dropping the output of a
		// short-lived process, so give the reader a chance to finish first.
		// Closing the PTY only on timeout unblocks a straggling read (e.g. a
		// grandchild still holds the slave side).
		waitErr = cmd.Wait()
		waitForReaders(&streamWg, func() { _ = proc.ClosePTY() })
		_ = proc.ClosePTY()
	} else {
		streamWg.Add(2)
		go streamOutput(stdoutR, false)
		go streamOutput(stderrR, true)

		waitErr = cmd.Wait()
		waitForReaders(&streamWg, func() { closeAll(stdoutR, stderrR) })
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
	result, err := s.execute(ctx, req.GetConfig(), executionLimits{})
	if err != nil {
		return nil, err
	}
	return &processv1.ExecuteResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, nil
}

// ExecuteWithLimits retains bounded output without interrupting pipe draining.
func (s *ProcessServer) ExecuteWithLimits(ctx context.Context, req *processv1.ExecuteWithLimitsRequest) (*processv1.ExecuteWithLimitsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.StdoutLimitBytes != nil && req.GetStdoutLimitBytes() == 0 || req.StderrLimitBytes != nil && req.GetStderrLimitBytes() == 0 {
		return nil, status.Error(codes.InvalidArgument, "output limits must be positive when specified")
	}
	timeLimit, err := requestedTimeLimit(req.GetTimeLimit())
	if err != nil {
		return nil, err
	}
	return s.execute(ctx, req.GetConfig(), executionLimits{
		timeLimit: timeLimit, stdoutBytes: req.GetStdoutLimitBytes(), stderrBytes: req.GetStderrLimitBytes(),
	})
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

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (run unaryExecution) finished(waitDone <-chan struct{}) bool {
	return channelClosed(waitDone) && channelClosed(run.stdout.done) && channelClosed(run.stderr.done)
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

	runErr, limited, err := s.waitExecution(ctx, unaryExecution{cmd, stdoutBuf, stderrBuf, drainGrace}, timeExpired)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	exitCode := int32(0)
	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			exitCode = exitStatus(exitErr)
		} else {
			return nil, mapCommandError(runErr, "failed to execute command")
		}
	}

	reason := processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED
	if limited {
		reason = processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED
	}

	return &processv1.ExecuteWithLimitsResponse{
		ExitCode:          exitCode,
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
func (s *ProcessServer) waitExecution(ctx context.Context, run unaryExecution, timeExpired <-chan time.Time) (error, bool, error) {
	waitDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = run.cmd.Wait()
		close(waitDone)
	}()
	limited := false
	pendingWait, pendingOut, pendingErr := waitDone, run.stdout.done, run.stderr.done
	ctxDone := ctx.Done()
	var pendingKill chan struct{}
	var cleanupDone, drainDone <-chan time.Time
	var cleanupTimer, drainTimer *time.Timer
	defer func() {
		if cleanupTimer != nil {
			cleanupTimer.Stop()
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
		case <-ctxDone:
			ctxDone, timeExpired = nil, nil
			if run.finished(waitDone) {
				continue
			}
			pendingKill = s.terminateExecution(run.cmd)
			cleanupTimer = time.NewTimer(executionCleanupTimeout)
			cleanupDone = cleanupTimer.C
		case <-timeExpired:
			ctxDone, timeExpired = nil, nil
			if run.finished(waitDone) {
				continue
			}
			limited = true
			pendingKill = s.terminateExecution(run.cmd)
			cleanupTimer = time.NewTimer(executionCleanupTimeout)
			cleanupDone = cleanupTimer.C
		case <-cleanupDone:
			if run.finished(waitDone) && (pendingKill == nil || channelClosed(pendingKill)) {
				cleanupDone = nil
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, false, status.FromContextError(err).Err()
			}
			return nil, false, status.Error(codes.Internal, "command cleanup exceeded its waiting limit")
		case <-drainDone:
			if channelClosed(run.stdout.done) && channelClosed(run.stderr.done) {
				drainDone = nil
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, false, status.FromContextError(err).Err()
			}
			return nil, false, status.Error(codes.Internal, "command output pipes did not finish draining")
		}
		if pendingOut == nil && pendingErr == nil && drainTimer != nil {
			drainTimer.Stop()
			drainDone = nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, status.FromContextError(err).Err()
	}
	if run.stdout.err != nil || run.stderr.err != nil {
		return nil, false, status.Error(codes.Internal, "failed to collect command output")
	}
	return runErr, limited, nil
}

func (s *ProcessServer) terminateExecution(cmd *exec.Cmd) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), executionCleanupTimeout)
		defer cancel()
		s.killProcessTree(cleanupCtx, cmd)
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
// child PID) so that grandchildren are reaped as well. The goroutine exits
// when ctx is done OR when the process exits (done closed), whichever comes
// first.
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
func (s *ProcessServer) killProcessGroupOnContextDone(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	go func() {
		select {
		case <-ctx.Done():
			// Skip the kill if the child has already been reaped by Wait;
			// its PID/PGID may have been reused by an unrelated process.
			select {
			case <-done:
				return
			default:
			}
			s.killProcessTree(context.Background(), cmd)
		case <-done:
		}
	}()
}

func (s *ProcessServer) killProcessTree(ctx context.Context, cmd *exec.Cmd) {
	// Wait may already have reaped the child while its output pipes drain.
	if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		return
	}
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

// waitForReaders blocks until every output reader in wg has finished. Readers
// get pipeDrainGrace to reach EOF on their own; after that forceClose is called
// to unblock any that are stuck (a grandchild may still hold the write side),
// and waitForReaders waits for them to return.
func waitForReaders(wg *sync.WaitGroup, forceClose func()) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pipeDrainGrace):
		forceClose()
		<-done
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
