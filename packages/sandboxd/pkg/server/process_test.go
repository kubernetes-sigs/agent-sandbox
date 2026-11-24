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
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
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

	grpcServer := grpc.NewServer()
	processv1.RegisterProcessServiceServer(grpcServer,
		NewProcessServer(rootDir, processmanager.NewProcessRegistry(), 0, logr.Discard()))
	return connectProcessClient(t, grpcServer)
}

func connectProcessClient(t *testing.T, grpcServer *grpc.Server) processv1.ProcessServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
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
	// pid is sandboxd's registry-assigned virtual process ID (from the
	// InitEvent), not an OS pid — clients never see real host pids.
	pid      int32
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	exitCode int32
}

// drainStart reads the stream until the ExitEvent and returns everything seen.
func drainStart(t *testing.T, stream processv1.ProcessService_StartClient) *startResult {
	t.Helper()
	res := &startResult{}
	sawExit := false
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch ev := msg.GetEvent().(type) {
		case *processv1.StartResponse_Init:
			require.False(t, sawExit, "InitEvent after ExitEvent")
			res.pid = ev.Init.GetProcessId()
		case *processv1.StartResponse_Stdout:
			res.stdout.Write(ev.Stdout)
		case *processv1.StartResponse_Stderr:
			res.stderr.Write(ev.Stderr)
		case *processv1.StartResponse_Exit:
			sawExit = true
			res.exitCode = ev.Exit.GetExitCode()
		}
	}
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

// skipIfGVisor skips the test when running under gVisor (runsc). gVisor does
// not faithfully reproduce all Linux process-group semantics — negative-PID
// signalling may not propagate to grandchildren — so tests that assert on
// grandchild termination cannot pass reliably there. Production uses a
// layered kill strategy (PGID kill + /proc descendant scan + direct PID
// kill) to compensate, but the regression tests still need a real kernel
// to validate the PGID path.
func skipIfGVisor(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return
	}
	if strings.Contains(string(data), "gvisor") || strings.Contains(string(data), "runsc") {
		t.Skip("process-group semantics unreliable under gVisor; skipping")
	}
}

// isProcessDead returns true if the process no longer exists (ESRCH) or is a
// zombie (state 'Z' in /proc/<pid>/stat). In containers without a proper init
// process, killed processes may become zombies if their parent doesn't reap
// them. We treat zombies as "dead" for the purposes of these tests since they
// are no longer executing and cannot cause harm.
func isProcessDead(pid int) bool {
	// First check if process exists at all
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}

	// On Linux, check if it's a zombie by reading /proc/<pid>/stat
	// Format: pid (comm) state ...
	// where state is 'Z' for zombie
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		// If we can't read /proc, assume process is dead (might not exist)
		return true
	}

	// Find the state field: it's after the closing paren of comm
	// comm can contain spaces and parens, so we find the last ')'
	statStr := string(data)
	lastParen := strings.LastIndex(statStr, ")")
	if lastParen < 0 || lastParen+2 >= len(statStr) {
		return false
	}

	state := statStr[lastParen+2]
	return state == 'Z'
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

func TestExecuteSignalExitCode(t *testing.T) {
	client := newProcessClient(t, t.TempDir())

	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "kill -9 $$"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(128+9), resp.GetExitCode())
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

func TestExecuteCwdUnderRegularFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	client := newProcessClient(t, root)

	cwd := "f.txt/child"
	_, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"true"}, Cwd: &cwd},
	})
	require.Equal(t, codes.NotFound, status.Code(err), err)
}

func TestExecuteCwdIsRegularFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	client := newProcessClient(t, root)

	cwd := "f.txt"
	_, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"true"}, Cwd: &cwd},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), err)
}

func TestExecuteCommandPathUnderRegularFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	client := newProcessClient(t, root)

	_, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"f.txt/prog"}},
	})
	require.Equal(t, codes.NotFound, status.Code(err), err)
	require.ErrorContains(t, err, "not a directory")
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

func TestStartClosesOutputPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc file descriptors")
	}
	// Disable GC so finalizers can't close the leaked readers and hide the leak.
	previousGC := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(previousGC) })
	client := newProcessClient(t, t.TempDir())

	pipeCount := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		require.NoError(t, err)
		count := 0
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if err == nil && strings.HasPrefix(target, "pipe:[") {
				count++
			}
		}
		return count
	}

	before := pipeCount()
	stream, err := client.Start(testCtx(t), &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{
			"sh", "-c", "printf stdout; printf stderr >&2",
		}},
	})
	require.NoError(t, err)
	result := drainStart(t, stream)
	require.Equal(t, "stdout", result.stdout.String())
	require.Equal(t, "stderr", result.stderr.String())
	require.Equal(t, int32(0), result.exitCode)
	require.Equal(t, before, pipeCount(), "completed streams must close both output readers")
}

// TestStartEndsWhenADetachedChildHoldsTheOutputPipe guards the reason
// pipeDrainGrace exists: a background child that inherits stdout, or the PTY,
// and keeps it open past the command's exit must not hold the stream open,
// whether it is silent or keeps writing to a client that reads more slowly
// than it writes.
func TestStartEndsWhenADetachedChildHoldsTheOutputPipe(t *testing.T) {
	tests := []struct {
		name       string
		background string
		// readDelay paces the client after each message; 2 ms per 4 KiB
		// chunk is about 2 MB/s, slower than yes writes.
		readDelay  time.Duration
		pty        bool
		wantPrefix string
	}{
		{name: "silent", background: "sleep 30", wantPrefix: "before"},
		{name: "writing to a slow client", background: "yes", readDelay: 2 * time.Millisecond, wantPrefix: "beforey\n"},
		// The child ignores the SIGHUP the session leader's exit sends it.
		{
			name: "writing to a slow client through a PTY", background: "trap '' HUP; yes",
			readDelay: 2 * time.Millisecond, pty: true, wantPrefix: "beforey\r\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			client := newProcessClient(t, root)
			pidFile := filepath.Join(root, "child.pid")
			t.Cleanup(func() {
				data, err := os.ReadFile(pidFile)
				if err != nil {
					return
				}
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})

			// The deadline is well past the bound, so a stream the child
			// keeps open fails here rather than at the test timeout.
			ctx, cancel := context.WithTimeout(context.Background(), pipeDrainGrace+15*time.Second)
			defer cancel()
			started := time.Now()
			req := &processv1.StartRequest{
				Config: &processv1.ProcessConfig{Command: []string{
					"sh", "-c", "printf before; " + tc.background + " & echo $! > child.pid; exit 0",
				}},
			}
			if tc.pty {
				req.Pty = &processv1.PTY{Cols: 80, Rows: 24}
			}
			stream, err := client.Start(ctx, req)
			require.NoError(t, err)

			var head bytes.Buffer
			var exit *int32
			for exit == nil {
				msg, err := stream.Recv()
				require.NoError(t, err, "stream still open after %v", time.Since(started))
				if out := msg.GetStdout(); head.Len() < 64 {
					head.Write(out)
				}
				if e := msg.GetExit(); e != nil {
					code := e.GetExitCode()
					exit = &code
				}
				time.Sleep(tc.readDelay)
			}
			require.Equal(t, int32(0), *exit)
			require.True(t, strings.HasPrefix(head.String(), tc.wantPrefix), "stdout starts %q", head.String())
			require.Less(t, time.Since(started), pipeDrainGrace+5*time.Second,
				"the background child's open pipe must not hold the stream open")
		})
	}
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

func TestWriteStdinEOFWithPTYRejected(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"cat"}},
		Pty:    &processv1.PTY{Cols: 80, Rows: 24},
	})
	require.NoError(t, err)
	pid := recvInit(t, stream)

	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{
		ProcessId: pid,
		Payload:   &processv1.WriteStdinRequest_Eof{Eof: &emptypb.Empty{}},
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "err: %v", err)

	// The terminal is still usable: Ctrl-D through the PTY ends cat normally.
	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{
		ProcessId: pid,
		Payload:   &processv1.WriteStdinRequest_Input{Input: []byte("\x04")},
	})
	require.NoError(t, err)
	res := drainStart(t, stream)
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
	require.Equal(t, int32(128+9), res.exitCode)
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

// ptyRaceIterations is how many short-lived PTY processes the race regression
// tests start. The races they guard against (#1803) only show up on some runs,
// so a single run would pass most of the time even without the fix.
const ptyRaceIterations = 50

// TestStartWithPTY_DeliversAllOutputOfShortLivedProcess guards against the PTY
// being closed as soon as the child is reaped, which discarded output the
// kernel still had buffered, so the client saw exit code 0 and truncated or
// empty output.
func TestStartWithPTY_DeliversAllOutputOfShortLivedProcess(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	for i := range ptyRaceIterations {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			stream, err := client.Start(ctx, &processv1.StartRequest{
				// The output is larger than the PTY buffer, so the child is
				// still writing its tail when it exits; that tail is what a
				// premature close drops.
				Config: &processv1.ProcessConfig{Command: []string{"seq", "1", "2000"}},
				Pty:    &processv1.PTY{Cols: 80, Rows: 24},
			})
			require.NoError(t, err)

			res := drainStart(t, stream)
			require.Equal(t, int32(0), res.exitCode)
			// The line discipline turns "\n" into "\r\n".
			lines := strings.Split(strings.TrimRight(res.stdout.String(), "\r\n"), "\r\n")
			require.Len(t, lines, 2000, "last line: %q", lines[len(lines)-1])
		})
	}
}

// gatedStartStream is a Start stream that models a client that stops reading.
// The first output event waits for release. Once stallAt bytes of output have
// been delivered, the next output event waits for stall. Init and Exit events
// pass through.
type gatedStartStream struct {
	grpc.ServerStream
	ctx     context.Context
	release chan struct{}
	stallAt int
	stall   time.Duration
	stalled bool
	stdout  bytes.Buffer
	exit    *int32
}

func (s *gatedStartStream) Context() context.Context { return s.ctx }

func (s *gatedStartStream) Send(resp *processv1.StartResponse) error {
	if out := resp.GetStdout(); out != nil {
		<-s.release
		if s.stall > 0 && !s.stalled && s.stdout.Len() >= s.stallAt {
			s.stalled = true
			time.Sleep(s.stall)
		}
		s.stdout.Write(out)
	}
	if exit := resp.GetExit(); exit != nil {
		code := exit.GetExitCode()
		s.exit = &code
	}
	return nil
}

// TestStartDeliversOutputTailToASlowConsumer guards against Start discarding
// output the child wrote before it exited when the client reads slowly. The
// reader takes one chunk and blocks in Send, the rest of the output stays in
// the pipe or PTY, and the child exits. The client then stays idle, or stalls
// partway through the tail, for longer than pipeDrainGrace. Start must still
// deliver every byte before the ExitEvent.
func TestStartDeliversOutputTailToASlowConsumer(t *testing.T) {
	const chunk = 64
	tests := []struct {
		name string
		pty  bool
		// total stays within what the pipe or PTY holds while the reader
		// waits, so the child exits. One page is the smallest pipe Linux
		// grants, even to a user over pipe-user-pages-soft.
		total int
		// idle is how long the first output event waits after the child
		// exits.
		idle time.Duration
		// stallAt and stall make the client stall again partway through the
		// tail.
		stallAt   int
		stall     time.Duration
		linuxOnly bool
	}{
		{name: "pipe", total: 4096, idle: pipeDrainGrace + 2*time.Second},
		// 1 KiB fits in the PTY on Linux and macOS.
		{name: "PTY", pty: true, total: 1024, idle: pipeDrainGrace + 2*time.Second},
		// 8 KiB is more than the line discipline's 4 KiB read buffer, which
		// is all that TIOCINQ counts. The rest waits in the PTY's flip buffer
		// (a Linux PTY holds 10 to 19 KiB in all, depending on the write
		// size). The client stalls at 6 KiB, past what the line discipline
		// held.
		{name: "PTY past the line discipline buffer", pty: true, total: 8 * 1024,
			stallAt: 6 * 1024, stall: pipeDrainGrace + 2*time.Second, linuxOnly: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.linuxOnly && runtime.GOOS != "linux" {
				t.Skip("sized for the Linux PTY buffers")
			}
			t.Parallel()
			root := t.TempDir()
			srv := NewProcessServer(root, processmanager.NewProcessRegistry(), chunk, logr.Discard())
			stream := &gatedStartStream{
				ctx: testCtx(t), release: make(chan struct{}), stallAt: tc.stallAt, stall: tc.stall,
			}
			req := &processv1.StartRequest{
				Config: &processv1.ProcessConfig{Command: []string{
					"sh", "-c", "head -c " + strconv.Itoa(tc.total) + " /dev/zero; touch exited",
				}},
			}
			if tc.pty {
				req.Pty = &processv1.PTY{Cols: 80, Rows: 24}
			}

			finished := make(chan struct{})
			var startErr error
			go func() {
				defer close(finished)
				startErr = srv.Start(req, stream)
			}()
			release := sync.OnceFunc(func() { close(stream.release) })
			t.Cleanup(func() {
				release()
				<-finished
			})

			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(root, "exited"))
				return err == nil
			}, 10*time.Second, 10*time.Millisecond, "child never finished writing")
			time.Sleep(tc.idle)
			release()
			<-finished

			require.NoError(t, startErr)
			require.NotNil(t, stream.exit, "no ExitEvent")
			require.Equal(t, int32(0), *stream.exit)
			require.Equal(t, tc.total, stream.stdout.Len(), "Start reported exit 0 with part of the output missing")
		})
	}
}

// TestWaitForReadersWaitsForASendThatBeginsAsTheGraceEnds guards against the
// grace closing the readers while a reader sends output written before the
// child exited. The reader begins its send after waitForReaders last checked
// for one, just as the grace runs out, so the timer and the tracker's change
// are both ready. select picks between them at random, so the case repeats.
func TestWaitForReadersWaitsForASendThatBeginsAsTheGraceEnds(t *testing.T) {
	t.Parallel()
	for range 64 {
		sends := newSendTracker()
		out := sends.track(nil, 1<<20)
		sends.childExited(0)
		var readers sync.WaitGroup
		readers.Add(1)

		timers := 0
		after := func(time.Duration) <-chan time.Time {
			timers++
			if timers == 1 {
				counted := sends.begin(out, 1)
				require.True(t, counted, "output written before the exit pauses the grace")
				go func() {
					time.Sleep(time.Millisecond)
					sends.end(counted)
				}()
			}
			expired := make(chan time.Time, 1)
			expired <- time.Now()
			return expired
		}
		var closedWhileSending bool
		waitForReaders(&readers, sends, after, func() {
			closedWhileSending, _ = sends.state()
			readers.Done()
		})
		require.False(t, closedWhileSending, "the grace closed the readers during a counted send")
	}
}

// TestStartWithPTY_InitialSizeVisibleImmediately guards against the requested
// window size being applied after the child started, so a child that queries
// it right away saw 0x0.
func TestStartWithPTY_InitialSizeVisibleImmediately(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx := testCtx(t)

	for i := range ptyRaceIterations {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			stream, err := client.Start(ctx, &processv1.StartRequest{
				// Exec stty directly rather than through sh so the size is
				// read as early as possible.
				Config: &processv1.ProcessConfig{Command: []string{"stty", "size"}},
				Pty:    &processv1.PTY{Cols: 80, Rows: 24},
			})
			require.NoError(t, err)

			res := drainStart(t, stream)
			require.Equal(t, int32(0), res.exitCode)
			require.Equal(t, "24 80\r\n", res.stdout.String())
		})
	}
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

// TestStart_KillsGrandchildrenOnCancel verifies that cancelling the stream
// context terminates not just the child process but also any grandchildren,
// preventing orphan process leaks.
func TestStart_KillsGrandchildrenOnCancel(t *testing.T) {
	skipIfGVisor(t)
	client := newProcessClient(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Capture the grandchild PID via a temp file rather than stdout, so
	// that parsing cannot be skipped and the PID is available regardless
	// of when stdout events are delivered.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// Disable job control (set +m) to ensure the backgrounded sleep stays
	// in the same process group as the parent shell. Background the sleep
	// so it becomes a grandchild of cmd, then write its PID ($!) to pidFile.
	// When we SIGKILL -PGID, both the sh process and the backgrounded sleep
	// must terminate.
	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"sh", "-c", "set +m; sleep 86400 & echo $! > " + pidFile + "; wait"},
		},
	})
	require.NoError(t, err)

	// Receive InitEvent to ensure the process is running.
	_ = recvInit(t, stream)

	// Wait for the PID file to appear (the shell writes it immediately on
	// entry). Polling is robust to CI scheduling delays.
	var grandchildPID int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return false
		}
		grandchildPID = pid
		return true
	}, 10*time.Second, 50*time.Millisecond, "grandchild PID file %q never populated", pidFile)

	// Cancel the context to trigger process group kill.
	cancel()

	// Wait for the stream to end. It may return EOF or a Canceled error.
	_, err = stream.Recv()
	require.Error(t, err)

	// Wait for the grandchild to die. The kill goroutine runs asynchronously
	// after ctx cancellation, so polling is needed to avoid a race between
	// the client observing the stream error and the server delivering SIGKILL
	// to the process group. 10s timeout accommodates slow CI runners.
	require.Eventually(t, func() bool {
		return isProcessDead(grandchildPID)
	}, 10*time.Second, 50*time.Millisecond, "grandchild process %d should have been killed", grandchildPID)
}

// TestExecute_KillsGrandchildrenOnCancel verifies that cancelling the context
// during Execute terminates the entire process group including grandchildren.
func TestExecute_KillsGrandchildrenOnCancel(t *testing.T) {
	skipIfGVisor(t)
	client := newProcessClient(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Capture the grandchild PID via a temp file rather than stdout.
	// Execute returns a Canceled error (and resp == nil) on cancellation,
	// so any stdout-based PID assertion would be vacuously skipped.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// Disable job control (set +m) to ensure the backgrounded sleep stays
	// in the same process group as the parent shell. Background the sleep
	// so it becomes a grandchild, then write its PID ($!) to pidFile.
	// When we SIGKILL -PGID, both the sh process and the backgrounded sleep
	// must terminate.
	cmd := "set +m; sleep 86400 & echo $! > " + pidFile + "; wait"

	// Start Execute in a goroutine so we can cancel mid-execution.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.Execute(ctx, &processv1.ExecuteRequest{
			Config: &processv1.ProcessConfig{
				Command: []string{"sh", "-c", cmd},
			},
		})
	}()

	// Wait for the PID file to appear (the shell writes it immediately on
	// entry, well before we need to cancel). Polling avoids the race that
	// a fixed time.Sleep introduces on loaded CI runners.
	var grandchildPID int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return false
		}
		grandchildPID = pid
		return true
	}, 10*time.Second, 50*time.Millisecond, "grandchild PID file %q never populated", pidFile)

	// Cancel the context to trigger process group kill.
	cancel()

	// Wait for Execute to return.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after context cancellation")
	}

	// Wait for the grandchild to die. Polling avoids a race between
	// Execute returning and SIGKILL propagating to the grandchild.
	require.Eventually(t, func() bool {
		return isProcessDead(grandchildPID)
	}, 10*time.Second, 50*time.Millisecond, "grandchild process %d should have been killed", grandchildPID)
}

func TestExecuteAbsoluteCwdWithFilesystemRoot(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	client := newProcessClient(t, "/")

	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"pwd"}, Cwd: &dir},
	})
	require.NoError(t, err)
	require.Equal(t, dir, strings.TrimSpace(string(resp.GetStdout())))
}
