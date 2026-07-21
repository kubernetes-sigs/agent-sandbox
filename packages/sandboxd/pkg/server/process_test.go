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
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// newProcessClient spins up an in-memory gRPC server hosting a ProcessServer
// rooted at rootDir and returns a connected client.
func newProcessClient(t *testing.T, rootDir string) processv1.ProcessServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	processv1.RegisterProcessServiceServer(grpcServer,
		NewProcessServer(rootDir, processmanager.NewProcessRegistry()))
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, conn.Close())
		grpcServer.GracefulStop()
		_ = lis.Close()
	})
	return processv1.NewProcessServiceClient(conn)
}

// startResult is the fully drained output of a Start stream.
type startResult struct {
	pid      int32
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	exitCode int32
}

// drainStart reads the stream until the ExitEvent and returns everything seen.
func drainStart(t *testing.T, stream processv1.ProcessService_StartClient) *startResult {
	t.Helper()
	res := &startResult{}
	sawInit, sawExit := false, false
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch ev := msg.GetEvent().(type) {
		case *processv1.StartResponse_Init:
			require.False(t, sawInit, "duplicate InitEvent")
			require.False(t, sawExit, "InitEvent after ExitEvent")
			sawInit = true
			res.pid = ev.Init.GetProcessId()
		case *processv1.StartResponse_Stdout:
			require.True(t, sawInit, "stdout before InitEvent")
			res.stdout.Write(ev.Stdout)
		case *processv1.StartResponse_Stderr:
			require.True(t, sawInit, "stderr before InitEvent")
			res.stderr.Write(ev.Stderr)
		case *processv1.StartResponse_Exit:
			require.True(t, sawInit, "ExitEvent before InitEvent")
			sawExit = true
			res.exitCode = ev.Exit.GetExitCode()
		}
	}
	require.True(t, sawInit, "no InitEvent received")
	require.True(t, sawExit, "no ExitEvent received")
	return res
}

// recvInit reads events until the InitEvent and returns the virtual PID.
func recvInit(t *testing.T, stream processv1.ProcessService_StartClient) int32 {
	t.Helper()
	for {
		msg, err := stream.Recv()
		require.NoError(t, err)
		if init := msg.GetInit(); init != nil {
			return init.GetProcessId()
		}
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestExecuteEcho(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"echo", "hello"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.GetExitCode())
	require.Equal(t, "hello\n", string(resp.GetStdout()))
	require.Empty(t, resp.GetStderr())
}

func TestExecuteExitCode(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "exit 3"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(3), resp.GetExitCode())
}

func TestExecuteEnvAndCwd(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	client := newProcessClient(t, root)

	cwd := "sub"
	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"sh", "-c", "echo $SANDBOX_TEST_VAR; pwd"},
			EnvVars: map[string]string{"SANDBOX_TEST_VAR": "sentinel"},
			Cwd:     &cwd,
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.GetExitCode())
	lines := strings.Split(strings.TrimSpace(string(resp.GetStdout())), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, "sentinel", lines[0])
	require.True(t, strings.HasSuffix(lines[1], "/sub"), "pwd output %q should end in /sub", lines[1])
}

func TestExecuteMissingCommand(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	_, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestExecuteCwdEscapeDenied(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	cwd := "../.."
	_, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"pwd"},
			Cwd:     &cwd,
		},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestStartStreamsStdoutStderrAndExit(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	stream, err := client.Start(testCtx(t), &processv1.StartRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"sh", "-c", "echo out; echo err 1>&2; exit 7"},
		},
	})
	require.NoError(t, err)

	res := drainStart(t, stream)
	require.Positive(t, res.pid)
	require.Equal(t, "out\n", res.stdout.String())
	require.Equal(t, "err\n", res.stderr.String())
	require.Equal(t, int32(7), res.exitCode)
}

func TestWriteStdinAndEOF(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"cat"}},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{
		ProcessId: pid,
		Payload:   &processv1.WriteStdinRequest_Input{Input: []byte("piped-through")},
	})
	require.NoError(t, err)

	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{
		ProcessId: pid,
		Payload:   &processv1.WriteStdinRequest_Eof{Eof: &emptypb.Empty{}},
	})
	require.NoError(t, err)

	res := drainStart(t, stream)
	require.Equal(t, "piped-through", res.stdout.String())
	require.Equal(t, int32(0), res.exitCode)
}

func TestWriteStdinNotFound(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	_, err := client.WriteStdin(testCtx(t), &processv1.WriteStdinRequest{
		ProcessId: 424242,
		Payload:   &processv1.WriteStdinRequest_Input{Input: []byte("x")},
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestSendSignalNotFound(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	_, err := client.SendSignal(testCtx(t), &processv1.SendSignalRequest{
		ProcessId: 424242,
		Signal:    processv1.Signal_SIGNAL_SIGTERM,
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestSendSignalUnspecifiedRejected(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sleep", "30"}},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: pid})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Clean up the sleeper and drain the stream.
	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{
		ProcessId: pid,
		Signal:    processv1.Signal_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	res := drainStart(t, stream)
	require.NotEqual(t, int32(0), res.exitCode)
}

func TestSendSignalTerminatesProcess(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sleep", "30"}},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{
		ProcessId: pid,
		Signal:    processv1.Signal_SIGNAL_SIGTERM,
	})
	require.NoError(t, err)

	res := drainStart(t, stream)
	require.NotEqual(t, int32(0), res.exitCode, "signal-killed process must not report success")
}

func TestResizeTTYWithoutPTYFails(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sleep", "30"}},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.ResizeTTY(ctx, &processv1.ResizeTTYRequest{ProcessId: pid, Cols: 80, Rows: 24})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{
		ProcessId: pid,
		Signal:    processv1.Signal_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	drainStart(t, stream)
}

func TestStartWithPTY(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{
			// `stty size` prints "rows cols" of the controlling terminal —
			// it only succeeds when a real PTY is attached.
			Command: []string{"sh", "-c", "stty size"},
		},
		Pty: &processv1.PTY{Cols: 80, Rows: 24},
	})
	require.NoError(t, err)

	res := drainStart(t, stream)
	require.Equal(t, int32(0), res.exitCode)
	require.Contains(t, res.stdout.String(), "24 80")
}

func TestResizeTTYWithPTY(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"cat"}},
		Pty:    &processv1.PTY{Cols: 80, Rows: 24},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.ResizeTTY(ctx, &processv1.ResizeTTYRequest{ProcessId: pid, Cols: 120, Rows: 40})
	require.NoError(t, err)

	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{
		ProcessId: pid,
		Signal:    processv1.Signal_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	drainStart(t, stream)
}
