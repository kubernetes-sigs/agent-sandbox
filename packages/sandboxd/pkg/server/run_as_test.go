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
	"os/exec"
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
	testRunAsPolicy = &RunAsPolicy{
		UIDs:       IDRange{Min: 10000, Max: 19999},
		GIDs:       IDRange{Min: 20000, Max: 29999},
		DefaultUID: 10001,
		DefaultGID: 20001,
	}
	testRunAsUser = &processv1.User{Uid: 12345, Gid: 23456, SupplementaryGids: []uint32{23457, 23458}}
	// testAuthDigest stands in for a token digest: Server.New refuses a
	// RunAsPolicy without one.
	testAuthDigest = make([]byte, AuthTokenDigestSize)
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
	// Effective capabilities, ambient capabilities, UID and NoNewPrivs.
	const procStatus = "Name:\tsandboxd\nUid:\t%[3]s\t%[3]s\t%[3]s\t%[3]s\nCapInh:\t0000000000000000\nCapPrm:\t%[1]s\nCapEff:\t%[1]s\nCapBnd:\t000001ffffffffff\nCapAmb:\t%[2]s\nNoNewPrivs:\t%[4]s\n"
	none := "0000000000000000"
	// CAP_CHOWN, CAP_KILL, CAP_SETGID and CAP_SETUID are bits 0, 5, 6 and 7.
	four := "00000000000000e1"
	require.NoError(t, checkRunAsCapabilities(fmt.Sprintf(procStatus, "000001ffffffffff", none, "0", "0")), "root")
	require.NoError(t, checkRunAsCapabilities(fmt.Sprintf(procStatus, four, none, "0", "0")), "root with only the four it needs")

	err := checkRunAsCapabilities(fmt.Sprintf(procStatus, none, none, "0", "0"))
	require.ErrorContains(t, err, "lacks CAP_SETUID, CAP_SETGID, CAP_KILL, CAP_CHOWN")
	// Root with CAP_KILL dropped could start a command it then cannot signal.
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, "000001ffffffffdf", none, "0", "0"))
	require.ErrorContains(t, err, "lacks CAP_KILL")
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, "00000000000000e0", none, "0", "0"))
	require.ErrorContains(t, err, "lacks CAP_CHOWN")
	err = checkRunAsCapabilities(fmt.Sprintf(procStatus, four, four, "0", "0"))
	require.ErrorContains(t, err, "ambient capabilities")
	// A daemon that is not root holds the capabilities through its binary. Its
	// child keeps them across the change of user and across executing that
	// binary, so a request whose command is sandboxd would start a second
	// daemon that has them. no_new_privs does not prevent that.
	for _, noNewPrivs := range []string{"0", "1"} {
		err = checkRunAsCapabilities(fmt.Sprintf(procStatus, four, none, "65534", noNewPrivs))
		require.ErrorContains(t, err, "needs sandboxd to run as root")
	}
	// A status without the Uid line is not taken for root.
	err = checkRunAsCapabilities("CapEff:\t" + four + "\nCapAmb:\t" + none + "\n")
	require.ErrorContains(t, err, "needs sandboxd to run as root")
}

// TestNewRunAsNeedsAuthToken verifies that a policy is refused without client
// authentication: the token is all that stops a command inside the sandbox
// from calling the API and naming another user.
func TestNewRunAsNeedsAuthToken(t *testing.T) {
	_, err := New(Options{RootDir: t.TempDir(), RunAs: testRunAsPolicy, Log: logr.Discard()})
	require.ErrorContains(t, err, "needs client authentication")
}

// TestNewRunAsValidatesDefaultUser verifies that the default user must be one
// a request could name, so it can be neither root nor outside the ranges.
func TestNewRunAsValidatesDefaultUser(t *testing.T) {
	fromZero := RunAsPolicy{UIDs: IDRange{Min: 0, Max: 19999}, GIDs: IDRange{Min: 0, Max: 29999}}
	for reason, policy := range map[string]RunAsPolicy{
		"default uid 0 is not allowed":     {UIDs: fromZero.UIDs, GIDs: fromZero.GIDs, DefaultUID: 0, DefaultGID: 20001},
		"default gid 0 is not allowed":     {UIDs: fromZero.UIDs, GIDs: fromZero.GIDs, DefaultUID: 10001, DefaultGID: 0},
		"default uid 9999 is not allowed":  {UIDs: testRunAsPolicy.UIDs, GIDs: testRunAsPolicy.GIDs, DefaultUID: 9999, DefaultGID: 20001},
		"default gid 30000 is not allowed": {UIDs: testRunAsPolicy.UIDs, GIDs: testRunAsPolicy.GIDs, DefaultUID: 10001, DefaultGID: 30000},
	} {
		_, err := New(Options{RootDir: t.TempDir(), RunAs: &policy, AuthTokenSHA256: testAuthDigest, Log: logr.Discard()})
		require.ErrorContains(t, err, reason)
	}
}

// TestNewRunAsNeedsPrivileges verifies that a daemon that may not change user
// fails when it is built, not on the first request that names a user.
func TestNewRunAsNeedsPrivileges(t *testing.T) {
	if checkRunAsPrivileges() == nil {
		t.Skip("the test process may run a command as another user, so there is no refusal to observe")
	}
	_, err := New(Options{RootDir: t.TempDir(), RunAs: testRunAsPolicy, AuthTokenSHA256: testAuthDigest, Log: logr.Discard()})
	require.ErrorContains(t, err, "running commands as another user")
	require.NotContains(t, err.Error(), "client authentication")
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
// test process could be a daemon with a RunAsPolicy: root on Linux with the
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

// runAsCases are the two ways a command gets its user from a daemon with a
// RunAsPolicy: the request names it, or the request names none and the command
// runs as the policy's default user. Neither is the daemon's own user, which
// here is the test process.
var runAsCases = []struct {
	name     string
	user     *processv1.User
	uid, gid uint32
}{
	{"named user", testRunAsUser, testRunAsUser.GetUid(), testRunAsUser.GetGid()},
	{"no user", nil, testRunAsPolicy.DefaultUID, testRunAsPolicy.DefaultGID},
}

// TestRunAsUser verifies on every path that the command runs as the named
// user and group, or as the default ones when the request names no user, and
// that a file it creates belongs to them.
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
				require.NotEqual(t, uint32(os.Getuid()), tc.uid, "the command must not run as the daemon")

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
		// The default user: only its own group.
		"20001\n": nil,
	} {
		resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
			Config: &processv1.ProcessConfig{Command: []string{"id", "-G"}, User: user},
		})
		require.NoError(t, err)
		require.Equal(t, want, string(resp.GetStdout()))
	}
}

// TestRunAsUserHoldsNoPrivileges guards on every path against the command
// inheriting the privilege the daemon used to start it, and against it gaining
// privilege later: under no_new_privs a setuid program or a file with
// capabilities gives it nothing.
func TestRunAsUserHoldsNoPrivileges(t *testing.T) {
	requireRunAsPrivileges(t)
	for _, path := range runAsPaths {
		for _, tc := range runAsCases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				res, err := path.run(t, newRunAsClient(t, runAsRootDir(t), testRunAsPolicy), &processv1.ProcessConfig{
					Command: []string{"grep", "-E", "^(Cap(Prm|Eff|Amb)|NoNewPrivs):", "/proc/self/status"},
					User:    tc.user,
				})
				require.NoError(t, err)
				require.Equal(t, "CapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\n", res.stdout)
			})
		}
	}
}

// TestRunAsStartsHelperAsDaemon verifies that the process the daemon forks for
// a command that runs as another user is sandboxd itself with no credential.
// A forked child that changed user could be stopped by that user before it
// executes, and the daemon waits in the fork until it does.
func TestRunAsStartsHelperAsDaemon(t *testing.T) {
	requireRunAsPrivileges(t)
	root := runAsRootDir(t)
	processServer := NewProcessServer(root, processmanager.NewProcessRegistry(), 0, logr.Discard())
	processServer.runAs = testRunAsPolicy
	cmd, err := processServer.buildCommand(testCtx(t), &processv1.ProcessConfig{Command: []string{"true"}, User: testRunAsUser})
	require.NoError(t, err)
	truePath := cmd.Path

	helper, err := processServer.startCommand(cmd)
	require.NoError(t, err)
	require.NoError(t, helper.result("start"))
	require.NoError(t, cmd.Wait())

	require.Nil(t, cmd.SysProcAttr.Credential)
	require.Equal(t, []string{}, cmd.Env)
	require.Equal(t, "/proc/self/exe", cmd.Path)
	require.Equal(t, []string{"sandboxd", RunAsHelperArg, "12345", "23456", "23457,23458", root, "", truePath, "true"}, cmd.Args)
}

// TestRunAsUserEnvironment verifies that the command gets the daemon's
// environment and the request's, with PWD naming its working directory, and
// that the run-as helper gets none of it. The helper starts as root, so a
// variable from the request must not reach it. GODEBUG=inittrace=1 makes any
// Go program that starts with it print to stderr, which the helper would do
// and the shell does not.
func TestRunAsUserEnvironment(t *testing.T) {
	requireRunAsPrivileges(t)
	t.Setenv("SANDBOXD_TEST_FROM_DAEMON", "daemon")
	root := runAsRootDir(t)
	want, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for _, path := range runAsPaths[:3] {
		t.Run(path.name, func(t *testing.T) {
			res, err := path.run(t, newRunAsClient(t, root, testRunAsPolicy), &processv1.ProcessConfig{
				Command: []string{"sh", "-c", `echo "$SANDBOXD_TEST_FROM_DAEMON $FROM_REQUEST $GODEBUG $PWD"`},
				EnvVars: map[string]string{"FROM_REQUEST": "request", "GODEBUG": "inittrace=1"},
				User:    testRunAsUser,
			})
			require.NoError(t, err)
			require.Equal(t, runAsResult{stdout: "daemon request inittrace=1 " + want + "\n"}, res)
		})
	}
}

// TestRunAsHelperServesOnlyRoot verifies that the run-as helper changes user
// only for the daemon, which is root. Anyone can execute the binary with the
// helper's arguments, and a binary with file capabilities would otherwise let
// them become any user.
func TestRunAsHelperServesOnlyRoot(t *testing.T) {
	requireRunAsPrivileges(t)
	statusRead, statusWrite, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = statusRead.Close() }()
	cmd := exec.Command("/proc/self/exe", RunAsHelperArg, "12345", "23456", "", "/", "", "/bin/true", "true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 10001, Gid: 20001}}
	environment, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer func() { _ = environment.Close() }()
	cmd.ExtraFiles = []*os.File{statusWrite, environment}
	require.NoError(t, cmd.Start())
	require.NoError(t, statusWrite.Close())

	report, err := io.ReadAll(statusRead)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s %d", helperStepUser, int(syscall.EPERM)), string(report))
	require.Error(t, cmd.Wait())
}

// TestRunAsUserStartFailures verifies that a command that cannot start as its
// user fails the request with the code a command of the daemon's own user
// gets, although the run-as helper is what meets the failure.
func TestRunAsUserStartFailures(t *testing.T) {
	requireRunAsPrivileges(t)
	root := runAsRootDir(t)
	// Only root may enter the directory or execute the script.
	require.NoError(t, os.Mkdir(filepath.Join(root, "roots-only"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "roots-only.sh"), []byte("#!/bin/sh\ntouch ran\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), nil, 0o644))
	require.NoError(t, os.Symlink("/", filepath.Join(root, "outside")))

	for _, tc := range []struct {
		name    string
		command []string
		cwd     string
		code    codes.Code
	}{
		{"cwd the user cannot enter", []string{"touch", "ran"}, "roots-only", codes.PermissionDenied},
		{"cwd that does not exist", []string{"touch", "ran"}, "missing", codes.NotFound},
		{"cwd that is a file", []string{"touch", "ran"}, "file", codes.InvalidArgument},
		{"cwd that leaves the root through a symlink", []string{"touch", "ran"}, "outside", codes.PermissionDenied},
		{"command the user cannot execute", []string{filepath.Join(root, "roots-only.sh")}, "", codes.PermissionDenied},
		{"command that does not exist", []string{filepath.Join(root, "missing")}, "", codes.NotFound},
	} {
		for _, path := range runAsPaths {
			t.Run(tc.name+"/"+path.name, func(t *testing.T) {
				config := &processv1.ProcessConfig{Command: tc.command, User: testRunAsUser}
				if tc.cwd != "" {
					config.Cwd = &tc.cwd
				}
				_, err := path.run(t, newRunAsClient(t, root, testRunAsPolicy), config)
				require.Equal(t, tc.code, status.Code(err), "%v", err)
				require.NoFileExists(t, filepath.Join(root, "ran"))
			})
		}
	}
}

// TestRunAsUserCwdBelowPrivateDirectory verifies that a command can start
// below a directory that only its user may enter. The daemon may be unable to
// look inside one, which is why the run-as helper resolves the working
// directory as the user.
func TestRunAsUserCwdBelowPrivateDirectory(t *testing.T) {
	requireRunAsPrivileges(t)
	root := runAsRootDir(t)
	private := filepath.Join(root, "private")
	nested := filepath.Join(private, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	want, err := filepath.EvalSymlinks(nested)
	require.NoError(t, err)
	// Give away the deeper directory first and take it back last. A test
	// process without CAP_DAC_OVERRIDE cannot reach below a directory that
	// belongs to the user, not even to remove it.
	uid, gid := int(testRunAsUser.GetUid()), int(testRunAsUser.GetGid())
	require.NoError(t, os.Chown(nested, uid, gid))
	require.NoError(t, os.Chown(private, uid, gid))
	t.Cleanup(func() {
		_ = os.Chown(private, os.Getuid(), os.Getgid())
		_ = os.Chown(nested, os.Getuid(), os.Getgid())
	})
	cwd := "private/nested"
	for _, path := range runAsPaths {
		t.Run(path.name, func(t *testing.T) {
			res, err := path.run(t, newRunAsClient(t, root, testRunAsPolicy), &processv1.ProcessConfig{
				Command: []string{"pwd", "-P"},
				Cwd:     &cwd,
				User:    testRunAsUser,
			})
			require.NoError(t, err)
			require.Equal(t, want+"\n", res.stdout)
		})
	}
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
			srv, err := New(Options{RootDir: runAsRootDir(t), RunAs: testRunAsPolicy, AuthTokenSHA256: testAuthDigest, Log: logr.Discard()})
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
