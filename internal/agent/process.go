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

package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "sigs.k8s.io/agent-sandbox/api/proto/v1"
)

type ProcessServer struct {
	pb.UnimplementedProcessServiceServer
}

func NewProcessServer() *ProcessServer {
	return &ProcessServer{}
}

// Execute runs a process synchronously and blocks until completion
func (s *ProcessServer) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	if len(req.Command) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command cannot be empty")
	}

	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	
	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range req.EnvVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	
	var exitCode int32 = 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = int32(exitError.ExitCode())
		} else {
			return nil, status.Errorf(codes.Internal, "failed to run command: %v", err)
		}
	}

	return &pb.ExecuteResponse{
		ExitCode: exitCode,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
	}, nil
}

// Start spawns an OS process and streams stdout and stderr in real-time
func (s *ProcessServer) Start(req *pb.StartRequest, stream pb.ProcessService_StartServer) error {
	if len(req.Command) == 0 {
		return status.Error(codes.InvalidArgument, "command cannot be empty")
	}

	ctx := stream.Context()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range req.EnvVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start command: %v", err)
	}

	// Channel to gather errors from pipe copy operations
	errChan := make(chan error, 2)

	// Stream stdout
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				sendErr := stream.Send(&pb.StreamOutputResponse{
					Stdout: string(buf[:n]),
				})
				if sendErr != nil {
					errChan <- sendErr
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					errChan <- err
				}
				break
			}
		}
		errChan <- nil
	}()

	// Stream stderr
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				sendErr := stream.Send(&pb.StreamOutputResponse{
					Stderr: string(buf[:n]),
				})
				if sendErr != nil {
					errChan <- sendErr
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					errChan <- err
				}
				break
			}
		}
		errChan <- nil
	}()

	// Wait for pipe readers to finish
	for i := 0; i < 2; i++ {
		if err := <-errChan; err != nil {
			_ = cmd.Process.Kill() // kill the process if streaming fails
			return status.Errorf(codes.Internal, "streaming error: %v", err)
		}
	}

	// Wait for command to complete to avoid zombie process
	if err := cmd.Wait(); err != nil {
		// We still return success if it completed, exit code is implicit in execution completion
		// for Start stream we just stream until EOF.
	}

	return nil
}

// SendSignal sends a Unix signal (like SIGINT, SIGKILL) to an active process
func (s *ProcessServer) SendSignal(ctx context.Context, req *pb.SendSignalRequest) (*emptypb.Empty, error) {
	if req.ProcessId <= 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid process ID")
	}

	proc, err := os.FindProcess(int(req.ProcessId))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "process %d not found: %v", req.ProcessId, err)
	}

	sig := syscall.Signal(req.Signal)
	if err := proc.Signal(sig); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send signal %d to process %d: %v", req.Signal, req.ProcessId, err)
	}

	return &emptypb.Empty{}, nil
}
