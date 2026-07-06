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
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type ProcessServer struct {
	processv1.UnimplementedProcessServiceServer
	rootDir  string
	registry *processmanager.ProcessRegistry
}

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

func (s *ProcessServer) Start(req *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
	config := req.GetConfig()
	if config == nil || len(config.GetCommand()) == 0 {
		return fmt.Errorf("command is required in StartRequest")
	}

	command := config.GetCommand()
	cmdName := command[0]
	cmdArgs := command[1:]

	cmd := exec.CommandContext(stream.Context(), cmdName, cmdArgs...)

	// Configure environment variables
	if len(config.GetEnvVars()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range config.GetEnvVars() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Configure working directory
	if config.GetCwd() != "" {
		sanitizedCwd, err := pathutil.SanitizePath(s.rootDir, config.GetCwd())
		if err != nil {
			return err
		}
		cmd.Dir = sanitizedCwd
	} else {
		cmd.Dir = s.rootDir
	}

	// Set process group for clean signal handling
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	pid := s.registry.NextPID()
	proc := &processmanager.ManagedProcess{
		ID:   pid,
		Cmd:  cmd,
		Done: make(chan struct{}),
	}

	var ptyFile *os.File
	var stdoutPipe, stderrPipe io.ReadCloser
	var stdinPipe io.WriteCloser
	var err error

	usePTY := req.GetPty() != nil
	if usePTY {
		ptyFile, err = pty.Start(cmd)
		if err != nil {
			return fmt.Errorf("failed to start command with PTY: %w", err)
		}
		proc.PTY = ptyFile
		proc.Stdin = ptyFile
	} else {
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to get stdout pipe: %w", err)
		}
		stderrPipe, err = cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("failed to get stderr pipe: %w", err)
		}
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to get stdin pipe: %w", err)
		}
		proc.Stdin = stdinPipe

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start command: %w", err)
		}
	}

	s.registry.Register(proc)
	defer s.registry.Remove(pid)

	// Set initial TTY size if requested
	if usePTY && req.GetPty().GetCols() > 0 && req.GetPty().GetRows() > 0 {
		_ = pty.Setsize(ptyFile, &pty.Winsize{
			Cols: uint16(req.GetPty().GetCols()),
			Rows: uint16(req.GetPty().GetRows()),
		})
	}

	// Send InitEvent
	if err := stream.Send(&processv1.StartResponse{
		Event: &processv1.StartResponse_Init{
			Init: &processv1.InitEvent{ProcessId: pid},
		},
	}); err != nil {
		return fmt.Errorf("failed to send InitEvent: %w", err)
	}

	var streamWg sync.WaitGroup
	var sendMu sync.Mutex

	// Helper to stream output chunks to client
	streamOutput := func(reader io.Reader, isStderr bool) {
		defer streamWg.Done()
		buf := make([]byte, 4096)
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

	if usePTY {
		streamWg.Add(1)
		go streamOutput(ptyFile, false)
	} else {
		streamWg.Add(2)
		go streamOutput(stdoutPipe, false)
		go streamOutput(stderrPipe, true)
	}

	// Wait for process to exit
	waitErr := cmd.Wait()
	if usePTY {
		_ = ptyFile.Close()
	}
	streamWg.Wait()

	exitCode := int32(0)
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			exitCode = -1
		}
	}

	proc.SetExitCode(exitCode)
	close(proc.Done)

	// Send ExitEvent
	return stream.Send(&processv1.StartResponse{
		Event: &processv1.StartResponse_Exit{
			Exit: &processv1.ExitEvent{ExitCode: exitCode},
		},
	})
}

func (s *ProcessServer) Execute(ctx context.Context, req *processv1.ExecuteRequest) (*processv1.ExecuteResponse, error) {
	config := req.GetConfig()
	if config == nil || len(config.GetCommand()) == 0 {
		return nil, fmt.Errorf("command is required in ExecuteRequest")
	}

	command := config.GetCommand()
	cmdName := command[0]
	cmdArgs := command[1:]

	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)

	if len(config.GetEnvVars()) > 0 {
		cmd.Env = os.Environ()
		for k, v := range config.GetEnvVars() {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if config.GetCwd() != "" {
		sanitizedCwd, err := pathutil.SanitizePath(s.rootDir, config.GetCwd())
		if err != nil {
			return nil, err
		}
		cmd.Dir = sanitizedCwd
	} else {
		cmd.Dir = s.rootDir
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	exitCode := int32(0)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return nil, fmt.Errorf("failed to execute command: %w", err)
		}
	}

	return &processv1.ExecuteResponse{
		ExitCode: exitCode,
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
	}, nil
}

func (s *ProcessServer) WriteStdin(ctx context.Context, req *processv1.WriteStdinRequest) (*processv1.WriteStdinResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, fmt.Errorf("process with ID %d not found", req.GetProcessId())
	}

	if req.GetEof() != nil {
		if proc.Stdin != nil {
			_ = proc.Stdin.Close()
		}
		return &processv1.WriteStdinResponse{}, nil
	}

	input := req.GetInput()
	if len(input) > 0 && proc.Stdin != nil {
		if _, err := proc.Stdin.Write(input); err != nil {
			return nil, fmt.Errorf("failed to write stdin: %w", err)
		}
	}

	return &processv1.WriteStdinResponse{}, nil
}

func (s *ProcessServer) SendSignal(ctx context.Context, req *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, fmt.Errorf("process with ID %d not found", req.GetProcessId())
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
		return nil, fmt.Errorf("unsupported or unspecified signal: %v", req.GetSignal())
	}

	if err := proc.Signal(sig); err != nil {
		return nil, fmt.Errorf("failed to send signal: %w", err)
	}

	return &processv1.SendSignalResponse{}, nil
}

func (s *ProcessServer) ResizeTTY(ctx context.Context, req *processv1.ResizeTTYRequest) (*processv1.ResizeTTYResponse, error) {
	proc, ok := s.registry.Get(req.GetProcessId())
	if !ok {
		return nil, fmt.Errorf("process with ID %d not found", req.GetProcessId())
	}

	if proc.PTY == nil {
		return nil, fmt.Errorf("process with ID %d does not have an active PTY", req.GetProcessId())
	}

	err := pty.Setsize(proc.PTY, &pty.Winsize{
		Rows: uint16(req.GetRows()),
		Cols: uint16(req.GetCols()),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to resize PTY: %w", err)
	}

	return &processv1.ResizeTTYResponse{}, nil
}
