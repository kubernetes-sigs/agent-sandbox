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
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

const (
	// streamChunkSize is the read buffer size for stdout/stderr streaming.
	streamChunkSize = 4096
	// executeWaitDelay bounds how long Execute waits for the command's I/O
	// pipes to drain after the process itself has exited (e.g. when a
	// grandchild inherited stdout and keeps it open).
	executeWaitDelay = 10 * time.Second
)

// ProcessServer implements the ProcessService gRPC API defined in
// packages/sandboxd/spec/process/v1/process.proto.
type ProcessServer struct {
	processv1.UnimplementedProcessServiceServer
	rootDir  string
	registry *processmanager.ProcessRegistry
}

// NewProcessServer builds a ProcessServer rooted at rootDir. A nil registry
// gets a fresh one, but callers normally share the daemon-wide registry so
// shutdown can signal every child.
func NewProcessServer(rootDir string, registry *processmanager.ProcessRegistry) *ProcessServer {
	if rootDir == "" {
		rootDir = "/"
	}
	if registry == nil {
		registry = processmanager.NewProcessRegistry()
	}
	return &ProcessServer{
		rootDir:  rootDir,
		registry: registry,
	}
}

// buildCommand translates a ProcessConfig into an exec.Cmd bound to ctx,
// with env merged over the daemon environment and cwd confined to rootDir.
func (s *ProcessServer) buildCommand(ctx context.Context, config *processv1.ProcessConfig) (*exec.Cmd, error) {
	if config == nil || len(config.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	command := config.GetCommand()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)

	if len(config.GetEnvVars()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range config.GetEnvVars() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if config.GetCwd() != "" {
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

	var ptyFile *os.File
	var stdoutPipe, stderrPipe io.ReadCloser
	var stdinPipe io.WriteCloser

	usePTY := req.GetPty() != nil
	if usePTY {
		// creack/pty sets Setsid on the child, which already places it in
		// its own process group (pgid == pid), so process-group signalling
		// works without Setpgid. Setting both would make fork fail with
		// EPERM (setpgid is illegal on a session leader).
		ptyFile, err = pty.Start(cmd)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to start command with PTY: %v", err)
		}
		proc.PTY = ptyFile
		proc.Stdin = ptyFile
	} else {
		// Put the child in its own process group so SendSignal reaches the
		// whole tree and shutdown sweeps don't leak grandchildren.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to get stdout pipe: %v", err)
		}
		stderrPipe, err = cmd.StderrPipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to get stderr pipe: %v", err)
		}
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to get stdin pipe: %v", err)
		}
		proc.Stdin = stdinPipe

		if err := cmd.Start(); err != nil {
			return status.Errorf(codes.Internal, "failed to start command: %v", err)
		}
	}

	// Register BEFORE sending InitEvent so a client that calls WriteStdin /
	// SendSignal immediately after receiving the event never races the
	// registry.
	s.registry.Register(proc)
	defer s.registry.Remove(pid)

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
		return status.Errorf(codes.Internal, "failed to send InitEvent: %v", err)
	}

	var streamWg sync.WaitGroup
	var sendMu sync.Mutex

	// streamOutput copies reader into stdout/stderr stream events until the
	// reader is exhausted (EOF on pipes, EIO/ErrClosed on a PTY).
	streamOutput := func(reader io.Reader, isStderr bool) {
		defer streamWg.Done()
		buf := make([]byte, streamChunkSize)
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
		_ = ptyFile.Close()
		streamWg.Wait()
	} else {
		streamWg.Add(2)
		go streamOutput(stdoutPipe, false)
		go streamOutput(stderrPipe, true)
		// For exec-managed pipes all reads must complete BEFORE cmd.Wait,
		// which closes them and would drop tail output.
		streamWg.Wait()
		waitErr = cmd.Wait()
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
	cmd.WaitDelay = executeWaitDelay

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	exitCode := int32(0)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return nil, status.Errorf(codes.Internal, "failed to execute command: %v", runErr)
		}
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

	if proc.PTY == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "process %d does not have an active PTY", req.GetProcessId())
	}

	if err := pty.Setsize(proc.PTY, &pty.Winsize{
		Rows: uint16(req.GetRows()),
		Cols: uint16(req.GetCols()),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resize PTY: %v", err)
	}

	return &processv1.ResizeTTYResponse{}, nil
}
