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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func TestExecuteWithLimitsOutput(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf XYZ >&2; exit 7"}},
		StdoutLimitBytes: proto.Uint64(3),
		StderrLimitBytes: proto.Uint64(3),
	})
	require.NoError(t, err)
	require.Equal(t, int32(7), resp.GetExitCode())
	require.Equal(t, "abc", string(resp.GetStdout()))
	require.Equal(t, "XYZ", string(resp.GetStderr()))
	require.True(t, resp.GetStdoutTruncated())
	require.False(t, resp.GetStderrTruncated())
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED, resp.GetTerminationReason())
	require.Positive(t, resp.GetElapsedDuration().AsDuration())
}

func TestExecuteWithLimitsOutputBoundaries(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	for _, tc := range []struct {
		name                       string
		out, errOut                *uint64
		wantOut, wantErr           string
		truncatedOut, truncatedErr bool
	}{
		{name: "unconfigured", wantOut: "abcdef", wantErr: "XYZ"},
		{name: "only stderr limited", errOut: proto.Uint64(2), wantOut: "abcdef", wantErr: "XY", truncatedErr: true},
		{name: "exact boundaries", out: proto.Uint64(6), errOut: proto.Uint64(3), wantOut: "abcdef", wantErr: "XYZ"},
		{name: "independent quotas", out: proto.Uint64(2), errOut: proto.Uint64(10), wantOut: "ab", wantErr: "XYZ", truncatedOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
				Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf XYZ >&2"}},
				StdoutLimitBytes: tc.out, StderrLimitBytes: tc.errOut,
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantOut, string(resp.GetStdout()))
			require.Equal(t, tc.wantErr, string(resp.GetStderr()))
			require.Equal(t, tc.truncatedOut, resp.GetStdoutTruncated())
			require.Equal(t, tc.truncatedErr, resp.GetStderrTruncated())
		})
	}
}

func TestExecuteWithLimitsDrainsOutput(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "seq 1 50000; printf done >&2"}},
		StdoutLimitBytes: proto.Uint64(4), StderrLimitBytes: proto.Uint64(4),
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode())
	require.Equal(t, "1\n2\n", string(resp.GetStdout()))
	require.Equal(t, "done", string(resp.GetStderr()))
	require.True(t, resp.GetStdoutTruncated())
	require.False(t, resp.GetStderrTruncated())
}

func TestExecuteWithLimitsOldService(t *testing.T) {
	// Register only the old method table, as a pre-extension daemon would.
	desc := processv1.ProcessService_ServiceDesc
	desc.Methods = nil
	for _, method := range processv1.ProcessService_ServiceDesc.Methods {
		if method.MethodName != "ExecuteWithLimits" {
			desc.Methods = append(desc.Methods, method)
		}
	}
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&desc, NewProcessServer(t.TempDir(), nil, 0, logr.Discard()))
	client := connectProcessClient(t, grpcServer)
	_, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "touch should-not-exist"}},
		StdoutLimitBytes: proto.Uint64(4),
	})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test ! -e should-not-exist"}},
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode(), "unsupported method must not execute or fall back")
}

func TestExecuteWithLimitsTimeoutAfterLeaderExit(t *testing.T) {
	skipIfGVisor(t)
	client := newProcessClient(t, t.TempDir())
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:    &processv1.ProcessConfig{Command: []string{"sh", "-c", "sleep 30 & printf started; exit 0"}},
		TimeLimit: durationpb.New(200 * time.Millisecond),
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode(), "preserve the actual direct-child exit status")
	require.Equal(t, "started", string(resp.GetStdout()))
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED, resp.GetTerminationReason())
}

func TestExecuteWithLimitsTimeout(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf started; exec sleep 30"}},
		TimeLimit:        durationpb.New(100 * time.Millisecond),
		StdoutLimitBytes: proto.Uint64(7),
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "execution limit must leave the RPC active")
	require.Equal(t, "started", string(resp.GetStdout()))
	require.Equal(t, int32(137), resp.GetExitCode())
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED, resp.GetTerminationReason())
	require.GreaterOrEqual(t, resp.GetElapsedDuration().AsDuration(), 100*time.Millisecond)
	resp, err = client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:    &processv1.ProcessConfig{Command: []string{"echo", "still available"}},
		TimeLimit: durationpb.New(time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, "still available\n", string(resp.GetStdout()))
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED, resp.GetTerminationReason())
}

func TestExecuteWithLimitsRPCDeadline(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{
		Config:    &processv1.ProcessConfig{Command: []string{"sleep", "30"}},
		TimeLimit: durationpb.New(2 * time.Second),
	})
	require.Nil(t, resp)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	resp, err = client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config: &processv1.ProcessConfig{Command: []string{"true"}},
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode())
}

func TestExecuteWithLimitsRPCCancelled(t *testing.T) {
	root := t.TempDir()
	client := newProcessClient(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{
			Config:    &processv1.ProcessConfig{Command: []string{"sh", "-c", "touch ready; exec sleep 30"}},
			TimeLimit: durationpb.New(20 * time.Second),
		})
		result <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "ready"))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-result:
		require.Equal(t, codes.Canceled, status.Code(err))
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not finish")
	}
}

func TestExecuteWithLimitsInvalidRequest(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	for _, tc := range []struct {
		name string
		req  *processv1.ExecuteWithLimitsRequest
	}{
		{"zero stdout", &processv1.ExecuteWithLimitsRequest{StdoutLimitBytes: proto.Uint64(0)}},
		{"zero stderr", &processv1.ExecuteWithLimitsRequest{StderrLimitBytes: proto.Uint64(0)}},
		{"zero time", &processv1.ExecuteWithLimitsRequest{TimeLimit: durationpb.New(0)}},
		{"negative time", &processv1.ExecuteWithLimitsRequest{TimeLimit: durationpb.New(-time.Second)}},
		{"overflow time", &processv1.ExecuteWithLimitsRequest{TimeLimit: &durationpb.Duration{Seconds: 10000000000}}},
		{"invalid duration", &processv1.ExecuteWithLimitsRequest{TimeLimit: &durationpb.Duration{Nanos: 1000000000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Config = &processv1.ProcessConfig{Command: []string{"touch", "invalid-request-ran"}}
			_, err := client.ExecuteWithLimits(testCtx(t), tc.req)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test ! -e invalid-request-ran"}},
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode(), "invalid requests must be rejected before execution")
}

func TestExecuteWithLimitsConcurrent(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	t.Run("executions", func(t *testing.T) {
		for _, limited := range []bool{true, false} {
			t.Run(strconv.FormatBool(limited), func(t *testing.T) {
				t.Parallel()
				cmd, limit := "printf independent; sleep 0.3; printf complete", 2*time.Second
				if limited {
					cmd, limit = "printf limited; exec sleep 30", 100*time.Millisecond
				}
				resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
					Config:    &processv1.ProcessConfig{Command: []string{"sh", "-c", cmd}},
					TimeLimit: durationpb.New(limit),
				})
				require.NoError(t, err)
				if limited {
					require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED, resp.GetTerminationReason())
				} else {
					require.Zero(t, resp.GetExitCode())
					require.Equal(t, "independentcomplete", string(resp.GetStdout()))
				}
			})
		}
	})
}

func TestExecuteWithLimitsIncompletePipeCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux setsid and /proc")
	}
	skipIfGVisor(t)
	_, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}
	root := t.TempDir()
	client := newProcessClient(t, root)
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(root, "escaped.pid"))
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				p, err := os.FindProcess(pid)
				if err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{
		Config:    &processv1.ProcessConfig{Command: []string{"sh", "-c", "setsid sh -c 'sleep 30 & echo $! > escaped.pid'; exec sleep 30"}},
		TimeLimit: durationpb.New(200 * time.Millisecond),
	})
	require.Equal(t, codes.Internal, status.Code(err), "unclosed output pipes must not look like complete cleanup")
	require.NoError(t, ctx.Err(), "cleanup must finish before the independent RPC deadline")
}

func TestProcessGroupCancellationAfterLeaderExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc to observe parent reaping")
	}
	skipIfGVisor(t)
	for _, rpc := range []string{"Start", "Execute", "ExecuteWithLimits"} {
		t.Run(rpc, func(t *testing.T) {
			root := t.TempDir()
			client := newProcessClient(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var childPID int
			t.Cleanup(func() {
				if childPID > 0 {
					p, err := os.FindProcess(childPID)
					if err == nil {
						_ = p.Kill()
					}
				}
			})
			config := &processv1.ProcessConfig{Command: []string{"sh", "-c", "sleep 30 & printf '%s %s' $$ $! > pids; exit 0"}}
			result := make(chan error, 1)
			switch rpc {
			case "Start":
				stream, err := client.Start(ctx, &processv1.StartRequest{Config: config})
				require.NoError(t, err)
				_, err = stream.Recv()
				require.NoError(t, err)
				go func() {
					for {
						if _, err := stream.Recv(); err != nil {
							result <- err
							return
						}
					}
				}()
			case "Execute":
				go func() { _, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: config}); result <- err }()
			case "ExecuteWithLimits":
				go func() {
					_, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: config})
					result <- err
				}()
			}
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(root, "pids"))
				if err != nil {
					return false
				}
				fields := strings.Fields(string(data))
				if len(fields) != 2 {
					return false
				}
				parentPID, err := strconv.Atoi(fields[0])
				if err != nil {
					return false
				}
				childPID, err = strconv.Atoi(fields[1])
				return err == nil && syscall.Kill(parentPID, 0) == syscall.ESRCH
			}, 2*time.Second, 10*time.Millisecond, "parent was not reaped before cancellation")
			cancel()
			select {
			case err := <-result:
				require.Equal(t, codes.Canceled, status.Code(err))
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled RPC did not end")
			}
			require.Eventually(t, func() bool { return isProcessDead(childPID) }, 2*time.Second, 10*time.Millisecond,
				"surviving same-group child must be terminated after its leader exits")
		})
	}
}

func TestExecuteDrainTimeoutTerminatesProcessGroup(t *testing.T) {
	skipIfGVisor(t)
	for _, rpc := range []string{"Execute", "ExecuteWithLimits"} {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(rpc+"/"+stream, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				srv, err := New(Options{RootDir: root, ExecutionPolicy: ExecutionPolicy{MaxTimeLimit: 20 * time.Second}})
				require.NoError(t, err)
				g := grpc.NewServer()
				srv.RegisterGRPC(g)
				client := connectProcessClient(t, g)
				pidFile := filepath.Join(root, "child.pid")
				t.Cleanup(func() {
					data, err := os.ReadFile(pidFile)
					if err != nil {
						return
					}
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil {
						return
					}
					p, err := os.FindProcess(pid)
					if err == nil {
						_ = p.Kill()
					}
				})
				command := "sleep 30 2>/dev/null & echo $! > child.pid; exit 0"
				if stream == "stderr" {
					command = "sleep 30 >/dev/null & echo $! > child.pid; exit 0"
				}
				config := &processv1.ProcessConfig{Command: []string{"sh", "-c", command}}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if rpc == "Execute" {
					resp, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: config})
					require.Nil(t, resp)
					require.Equal(t, codes.Internal, status.Code(err))
				} else {
					resp, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: config})
					require.Nil(t, resp)
					require.Equal(t, codes.Internal, status.Code(err))
				}
				require.NoError(t, ctx.Err(), "drain failure must complete within the RPC budget")
				data, err := os.ReadFile(pidFile)
				require.NoError(t, err)
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					if runtime.GOOS == "linux" {
						return isProcessDead(pid)
					}
					return syscall.Kill(pid, 0) == syscall.ESRCH
				}, 2*time.Second, 10*time.Millisecond, "drain failure left a same-group child running")
				resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
					Config: &processv1.ProcessConfig{Command: []string{"true"}},
				})
				require.NoError(t, err)
				require.Zero(t, resp.GetExitCode())
			})
		}
	}
}
