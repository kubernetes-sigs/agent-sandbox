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
		ptyFile, err = pty.Start(cmd)
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

	// Set initial TTY size if requested.
	if usePTY && req.GetPty().GetCols() > 0 && req.GetPty().GetRows() > 0 {
		_ = pty.Setsize(ptyFile, &pty.Winsize{
			Cols: uint16(req.GetPty().GetCols()),
			Rows: uint16(req.GetPty().GetRows()),
		})
	}

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
		// For a PTY the reader unblocks with EIO once the child exits, so
		// reaping first is safe; closing the PTY afterwards unblocks any
		// straggling read (e.g. a grandchild still holds the slave side).
		waitErr = cmd.Wait()
		_ = proc.ClosePTY()
		streamWg.Wait()
	} else {
		streamWg.Add(2)
		go streamOutput(stdoutR, false)
		go streamOutput(stderrR, true)

		waitErr = cmd.Wait()

		readersDone := make(chan struct{})
		go func() {
			streamWg.Wait()
			close(readersDone)
		}()
		select {
		case <-readersDone:
		case <-time.After(pipeDrainGrace):
			closeAll(stdoutR, stderrR)
			<-readersDone
		}
	}

	exitCode := int32(0)
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
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
	cmd, err := s.buildCommand(ctx, req.GetConfig())
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Bail out early if the caller's context is already cancelled.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	if err := cmd.Start(); err != nil {
		return nil, mapCommandError(err, "failed to execute command")
	}

	// Kill the entire process group on context cancellation. Use a local
	// done channel since Execute does not register in the process registry.
	execDone := make(chan struct{})
	defer close(execDone)
	s.killProcessGroupOnContextDone(ctx, cmd, execDone)

	runErr := cmd.Wait()
	exitCode := int32(0)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return nil, mapCommandError(runErr, "failed to execute command")
		}
	}

	// The command may have been killed by killProcessGroupOnContextDone
	// because the caller's context was cancelled. Surface that as a gRPC
	// cancellation rather than a normal (successful) response with a
	// non-zero exit code.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	return &processv1.ExecuteResponse{
		ExitCode: exitCode,
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
	}, nil
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
			pid := cmd.Process.Pid

			// Strategy 1: process-group kill (real Linux; reaches all
			// descendants in one syscall when the runtime honours PGIDs).
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				s.log.Error(err, "failed to kill process group on context cancellation", "pgid", pid)
			}

			// Strategy 2: walk /proc to find and kill descendants. This
			// is the fallback for runtimes where negative-PID signalling
			// does not reach the whole tree (e.g. gVisor). On real Linux
			// this is mostly a no-op — strategy 1 already killed them —
			// and any remaining Kill calls return ESRCH which we ignore.
			s.killDescendants(pid)

			// Strategy 3: direct PID kill as last resort.
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				s.log.Error(err, "failed to kill child process on context cancellation", "pid", pid)
			}
		case <-done:
		}
	}()
}

// killDescendants recursively kills all descendant processes of rootPID by
// walking /proc. On real Linux, killProcessGroupOnContextDone's strategy 1
// already killed the tree, so the Kill calls below return ESRCH and are
// silently skipped. In runtimes like gVisor where PGID kill does not
// propagate, this ensures grandchildren do not outlive the parent.
func (s *ProcessServer) killDescendants(rootPID int) {
	for _, cpid := range readProcChildren(rootPID) {
		// Recurse first so we kill leaves before their parents — otherwise
		// a child reaped mid-walk could reparent its children to init
		// before we get a chance to signal them.
		s.killDescendants(cpid)
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
func readProcChildren(pid int) []int {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{})
	var children []int
	for _, entry := range entries {
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
