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
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// The policy and the user the tests name. The IDs need no passwd or group
// entry.
var (
	testRunAsPolicy = &RunAsPolicy{UIDs: IDRange{Min: 10000, Max: 19999}, GIDs: IDRange{Min: 20000, Max: 29999}}
	testRunAsUser   = &processv1.User{Uid: 12345, Gid: 23456, SupplementaryGids: []uint32{23457, 23458}}
)

// newRunAsClient is newProcessClient for a ProcessServer with a RunAsPolicy.
// It skips Server.New, so it also works where the test process may not change
// user.
func newRunAsClient(t *testing.T, rootDir string, policy *RunAsPolicy) processv1.ProcessServiceClient {
	t.Helper()
	processServer := NewProcessServer(rootDir, processmanager.NewProcessRegistry(), 0, logr.Discard())
	processServer.runAs = policy
	grpcServer := grpc.NewServer()
	processv1.RegisterProcessServiceServer(grpcServer, processServer)
	return connectProcessClient(t, grpcServer)
}

// runAsResult is what a command left behind on any of the runAsPaths.
type runAsResult struct {
	stdout, stderr string
	exitCode       int32
}

func startAndDrain(t *testing.T, client processv1.ProcessServiceClient, req *processv1.StartRequest) (runAsResult, error) {
	t.Helper()
	stream, err := client.Start(testCtx(t), req)
	require.NoError(t, err)
	var res runAsResult
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return runAsResult{}, err
		}
		res.stdout += string(msg.GetStdout())
		res.stderr += string(msg.GetStderr())
		res.exitCode = msg.GetExit().GetExitCode()
	}
}

// runAsPaths are the ways a ProcessConfig reaches a command. Each one builds
// the command's SysProcAttr in a different place, so each must carry the user.
var runAsPaths = []struct {
	name string
	run  func(*testing.T, processv1.ProcessServiceClient, *processv1.ProcessConfig) (runAsResult, error)
}{
	{"Execute", func(t *testing.T, client processv1.ProcessServiceClient, config *processv1.ProcessConfig) (runAsResult, error) {
		resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{Config: config})
		return runAsResult{string(resp.GetStdout()), string(resp.GetStderr()), resp.GetExitCode()}, err
	}},
	{"ExecuteWithLimits", func(t *testing.T, client processv1.ProcessServiceClient, config *processv1.ProcessConfig) (runAsResult, error) {
		resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{Config: config})
		return runAsResult{string(resp.GetStdout()), string(resp.GetStderr()), resp.GetExitCode()}, err
	}},
	{"Start", func(t *testing.T, client processv1.ProcessServiceClient, config *processv1.ProcessConfig) (runAsResult, error) {
		return startAndDrain(t, client, &processv1.StartRequest{Config: config})
	}},
	{"StartWithPTY", func(t *testing.T, client processv1.ProcessServiceClient, config *processv1.ProcessConfig) (runAsResult, error) {
		res, err := startAndDrain(t, client, &processv1.StartRequest{Config: config, Pty: &processv1.PTY{Cols: 80, Rows: 24}})
		// The line discipline turns "\n" into "\r\n".
		res.stdout = strings.ReplaceAll(res.stdout, "\r\n", "\n")
		return res, err
	}},
}

func TestParseIDRange(t *testing.T) {
	for value, want := range map[string]IDRange{
		"1000-1999":    {Min: 1000, Max: 1999},
		"7-7":          {Min: 7, Max: 7},
		"0-4294967294": {Min: 0, Max: 4294967294},
	} {
		got, err := ParseIDRange(value)
		require.NoError(t, err, value)
		require.Equal(t, want, got, value)
	}
	for _, value := range []string{"", "1000", "2-1", "a-b", "-5", "1-", "1-2-3", "1-4294967295", "1-4294967296"} {
		_, err := ParseIDRange(value)
		require.Error(t, err, value)
	}
}

func TestCheckRunAsCapabilities(t *testing.T) {
	const procStatus = "Name:\tsandboxd\nCapInh:\t0000000000000000\nCapPrm:\t%[1]s\nCapEff:\t%[1]s\nCapBnd:\t000001ffffffffff\nCapAmb:\t%[2]s\n"
	none := "0000000000000000"
	// CAP_CHOWN, CAP_KILL, CAP_SETGID and CAP_SETUID are bits 0, 5, 6 and 7.
	require.NoError(t, checkRunAsCapabilities(fmt.Sprintf(procStatus, "000001ffffffffff", none)), "root")
	require.NoError(t, checkRunAsCapabilities(fmt.Sprintf(procStatus, "00000000000000e1", none)), "only the four it needs")

	err := checkRunAsCapabilities(fmt.Sprintf(procStatus, none, none))
	require.ErrorContains(t, err, "lacks CAP_SETUID, CAP_SETGID, CAP_KILL, CAP_CHOWN")
	// Root with CAP_KILL dropped could start a command it then cannot signal.
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, "000001ffffffffdf", none))
	require.ErrorContains(t, err, "lacks CAP_KILL")
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, "00000000000000e0", none))
	require.ErrorContains(t, err, "lacks CAP_CHOWN")
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, "00000000000000e1", "00000000000000e1"))
	require.ErrorContains(t, err, "ambient capabilities")
}

// TestNewRunAsNeedsPrivileges verifies that a daemon that may not change user
// fails when it is built, not on the first request that names a user.
func TestNewRunAsNeedsPrivileges(t *testing.T) {
	if checkRunAsPrivileges() == nil {
		t.Skip("the test process may run a command as another user, so there is no refusal to observe")
	}
	_, err := New(Options{RootDir: t.TempDir(), RunAs: testRunAsPolicy, Log: logr.Discard()})
	require.ErrorContains(t, err, "running commands as another user")
}

// requireRefused asserts that no path runs config's command, and that each
// one answers PERMISSION_DENIED with a message containing reason.
func requireRefused(t *testing.T, policy *RunAsPolicy, user *processv1.User, reason string) {
	t.Helper()
	for _, path := range runAsPaths {
		root := t.TempDir()
		_, err := path.run(t, newRunAsClient(t, root, policy), &processv1.ProcessConfig{
			Command: []string{"touch", "ran"},
			User:    user,
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err), path.name)
		require.Contains(t, status.Convert(err).Message(), reason, path.name)
		require.NoFileExists(t, filepath.Join(root, "ran"), path.name)
	}
}

func TestRunAsRefusedWithoutPolicy(t *testing.T) {
	requireRefused(t, nil, testRunAsUser, "does not run commands as another user")
}

func TestRunAsRefusesUserOutsidePolicy(t *testing.T) {
	// 0 is refused even by a policy whose ranges include it.
	fromZero := &RunAsPolicy{UIDs: IDRange{Min: 0, Max: 19999}, GIDs: IDRange{Min: 0, Max: 29999}}
	for _, tc := range []struct {
		name   string
		policy *RunAsPolicy
		user   *processv1.User
		reason string
	}{
		{"uid below the range", testRunAsPolicy, &processv1.User{Uid: 9999, Gid: 23456}, "uid 9999 is not allowed"},
		{"uid above the range", testRunAsPolicy, &processv1.User{Uid: 20000, Gid: 23456}, "uid 20000 is not allowed"},
		{"gid below the range", testRunAsPolicy, &processv1.User{Uid: 12345, Gid: 19999}, "gid 19999 is not allowed"},
		{"gid above the range", testRunAsPolicy, &processv1.User{Uid: 12345, Gid: 30000}, "gid 30000 is not allowed"},
		{"supplementary gid outside the range", testRunAsPolicy, &processv1.User{Uid: 12345, Gid: 23456, SupplementaryGids: []uint32{23457, 30000}}, "gid 30000 is not allowed"},
		{"uid 0", fromZero, &processv1.User{Uid: 0, Gid: 23456}, "uid 0 is not allowed"},
		{"gid 0", fromZero, &processv1.User{Uid: 12345, Gid: 0}, "gid 0 is not allowed"},
		{"supplementary gid 0", fromZero, &processv1.User{Uid: 12345, Gid: 23456, SupplementaryGids: []uint32{0}}, "gid 0 is not allowed"},
		{"empty user", fromZero, &processv1.User{}, "uid 0 is not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireRefused(t, tc.policy, tc.user, tc.reason)
		})
	}
}

// requireRunAsPrivileges skips a test that really changes user unless the
// test process could be a daemon with a RunAsPolicy: root on Linux, or the
// four capabilities.
func requireRunAsPrivileges(t *testing.T) {
	t.Helper()
	if err := checkRunAsPrivileges(); err != nil {
		t.Skipf("the test process cannot run a command as another user: %v", err)
	}
}

// runAsRootDir returns a sandbox root that another user can enter and write
// to. t.TempDir alone is reachable only by the test's own user.
func runAsRootDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Chmod(filepath.Dir(root), 0o755))
	require.NoError(t, os.Chmod(root, 0o777))
	return root
}

// runAsCases pairs every test that names a user with its control: the same
// command without a user runs as the daemon, which here is the test process.
var runAsCases = []struct {
	name     string
	user     *processv1.User
	uid, gid uint32
}{
	{"named user", testRunAsUser, testRunAsUser.GetUid(), testRunAsUser.GetGid()},
	{"no user", nil, uint32(os.Getuid()), uint32(os.Getgid())},
}

// TestRunAsUser verifies on every path that the command runs as the named
// user and group, and that a file it creates belongs to them.
func TestRunAsUser(t *testing.T) {
	requireRunAsPrivileges(t)
	for _, path := range runAsPaths {
		for _, tc := range runAsCases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				root := runAsRootDir(t)
				res, err := path.run(t, newRunAsClient(t, root, testRunAsPolicy), &processv1.ProcessConfig{
					Command: []string{"sh", "-c", "id -u && id -g && touch owned"},
					User:    tc.user,
				})
				require.NoError(t, err)
				require.Equal(t, int32(0), res.exitCode, res.stderr)
				require.Equal(t, fmt.Sprintf("%d\n%d\n", tc.uid, tc.gid), res.stdout)

				info, err := os.Stat(filepath.Join(root, "owned"))
				require.NoError(t, err)
				stat := info.Sys().(*syscall.Stat_t)
				require.Equal(t, tc.uid, stat.Uid)
				require.Equal(t, tc.gid, stat.Gid)
			})
		}
	}
}

// TestRunAsUserGroups verifies that the command gets exactly the groups the
// request names and none of the daemon's own.
func TestRunAsUserGroups(t *testing.T) {
	requireRunAsPrivileges(t)
	client := newRunAsClient(t, runAsRootDir(t), testRunAsPolicy)
	for want, user := range map[string]*processv1.User{
		"23456 23457 23458\n": testRunAsUser,
		"23456\n":             {Uid: 12345, Gid: 23456},
	} {
		resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
			Config: &processv1.ProcessConfig{Command: []string{"id", "-G"}, User: user},
		})
		require.NoError(t, err)
		require.Equal(t, want, string(resp.GetStdout()))
	}
}

// TestRunAsUserHoldsNoCapabilities guards against the command inheriting the
// privilege the daemon used to start it, which would let it become root.
func TestRunAsUserHoldsNoCapabilities(t *testing.T) {
	requireRunAsPrivileges(t)
	client := newRunAsClient(t, runAsRootDir(t), testRunAsPolicy)
	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"grep", "-E", "^Cap(Prm|Eff|Amb):", "/proc/self/status"},
			User:    testRunAsUser,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "CapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\n", string(resp.GetStdout()))
}

// TestRunAsUserOwnsStdio verifies that the command can open its own output by
// path. The pipes and the terminal are created by the daemon, so this fails
// with "permission denied" unless they are handed to the named user.
func TestRunAsUserOwnsStdio(t *testing.T) {
	requireRunAsPrivileges(t)
	for _, path := range runAsPaths[:3] {
		t.Run(path.name, func(t *testing.T) {
			res, err := path.run(t, newRunAsClient(t, runAsRootDir(t), testRunAsPolicy), &processv1.ProcessConfig{
				Command: []string{"sh", "-c", "echo out >/dev/stdout && echo err >/dev/stderr"},
				User:    testRunAsUser,
			})
			require.NoError(t, err)
			require.Equal(t, runAsResult{stdout: "out\n", stderr: "err\n"}, res)
		})
	}
	for _, tc := range runAsCases {
		t.Run("StartWithPTY/"+tc.name, func(t *testing.T) {
			path := runAsPaths[3]
			res, err := path.run(t, newRunAsClient(t, runAsRootDir(t), testRunAsPolicy), &processv1.ProcessConfig{
				// Print the terminal's owner, open it by path, and read its
				// size, which was set before the command started.
				Command: []string{"sh", "-c", `stat -c %u "$(tty)" && : <>"$(tty)" && stty size`},
				User:    tc.user,
			})
			require.NoError(t, err)
			require.Equal(t, runAsResult{stdout: fmt.Sprintf("%d\n24 80\n", tc.uid)}, res)
		})
	}
}

// startSleeper starts a command that prints its user ID and OS process ID and
// then sleeps, and returns once it has read both.
func startSleeper(ctx context.Context, t *testing.T, client processv1.ProcessServiceClient, user *processv1.User) (stream processv1.ProcessService_StartClient, virtualPID int32, uid uint32, osPID int) {
	t.Helper()
	stream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "id -u && echo $$ && exec sleep 300"}, User: user},
	})
	require.NoError(t, err)
	virtualPID = recvInit(t, stream)
	var stdout string
	for strings.Count(stdout, "\n") < 2 {
		msg, err := stream.Recv()
		require.NoError(t, err)
		stdout += string(msg.GetStdout())
	}
	lines := strings.Split(stdout, "\n")
	parsedUID, err := strconv.ParseUint(lines[0], 10, 32)
	require.NoError(t, err)
	osPID, err = strconv.Atoi(lines[1])
	require.NoError(t, err)
	return stream, virtualPID, uint32(parsedUID), osPID
}

// TestRunAsUserIsSignalled verifies that sandboxd still reaches a command
// after it changed user: through SendSignal, when its stream is cancelled,
// and in the shutdown sweep.
func TestRunAsUserIsSignalled(t *testing.T) {
	requireRunAsPrivileges(t)
	for _, tc := range runAsCases {
		t.Run("SendSignal/"+tc.name, func(t *testing.T) {
			client := newRunAsClient(t, runAsRootDir(t), testRunAsPolicy)
			ctx := testCtx(t)
			stream, virtualPID, uid, _ := startSleeper(ctx, t, client, tc.user)
			require.Equal(t, tc.uid, uid)

			_, err := client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: virtualPID, Signal: processv1.Signal_SIGNAL_SIGTERM})
			require.NoError(t, err)
			require.Equal(t, int32(128+15), drainStart(t, stream).exitCode)
		})

		t.Run("cancelled stream/"+tc.name, func(t *testing.T) {
			client := newRunAsClient(t, runAsRootDir(t), testRunAsPolicy)
			ctx, cancel := context.WithCancel(testCtx(t))
			defer cancel()
			_, _, uid, osPID := startSleeper(ctx, t, client, tc.user)
			require.Equal(t, tc.uid, uid)

			cancel()
			require.Eventually(t, func() bool { return isProcessDead(osPID) }, 10*time.Second, 20*time.Millisecond)
		})

		t.Run("shutdown sweep/"+tc.name, func(t *testing.T) {
			// Server.New also proves that a privileged daemon accepts the policy.
			srv, err := New(Options{RootDir: runAsRootDir(t), RunAs: testRunAsPolicy, Log: logr.Discard()})
			require.NoError(t, err)
			grpcServer := grpc.NewServer()
			srv.RegisterGRPC(grpcServer)
			stream, _, uid, _ := startSleeper(testCtx(t), t, connectProcessClient(t, grpcServer), tc.user)
			require.Equal(t, tc.uid, uid)

			srv.ShutdownProcesses(testCtx(t))
			require.Equal(t, int32(128+15), drainStart(t, stream).exitCode)
		})
	}
}
