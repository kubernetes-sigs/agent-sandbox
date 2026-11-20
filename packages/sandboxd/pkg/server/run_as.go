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
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// IDRange is an inclusive range of numeric user or group IDs.
type IDRange struct {
	Min, Max uint32
}

// ParseIDRange parses a "<min>-<max>" flag value such as "1000-1999".
func ParseIDRange(value string) (IDRange, error) {
	lo, hi, ok := strings.Cut(value, "-")
	if !ok {
		return IDRange{}, fmt.Errorf("ID range %q must be <min>-<max>", value)
	}
	minID, err := strconv.ParseUint(lo, 10, 32)
	if err != nil {
		return IDRange{}, fmt.Errorf("ID range %q: %w", value, err)
	}
	maxID, err := strconv.ParseUint(hi, 10, 32)
	if err != nil {
		return IDRange{}, fmt.Errorf("ID range %q: %w", value, err)
	}
	// The kernel reserves the highest ID to mean "no change".
	if minID > maxID || maxID == math.MaxUint32 {
		return IDRange{}, fmt.Errorf("ID range %q must have min <= max <= %d", value, uint32(math.MaxUint32-1))
	}
	return IDRange{Min: uint32(minID), Max: uint32(maxID)}, nil
}

// contains reports whether a request may name id. It never allows 0, so no
// range lets a caller ask for root.
func (r IDRange) contains(id uint32) bool {
	return id != 0 && id >= r.Min && id <= r.Max
}

// RunAsPolicy lists the users and groups a request may name in
// ProcessConfig.user, and the user of a command whose request names none. A
// nil policy refuses every named user.
type RunAsPolicy struct {
	UIDs IDRange
	GIDs IDRange
	// DefaultUID and DefaultGID are the user and group of a command whose
	// request names no user. With a policy, no command runs as the daemon's
	// own user, so leaving the user out is not a way around the ranges.
	DefaultUID, DefaultGID uint32
}

// Validate reports whether the default user is one a request could name.
func (p *RunAsPolicy) Validate() error {
	if !p.UIDs.contains(p.DefaultUID) {
		return fmt.Errorf("default uid %d is not allowed", p.DefaultUID)
	}
	if !p.GIDs.contains(p.DefaultGID) {
		return fmt.Errorf("default gid %d is not allowed", p.DefaultGID)
	}
	return nil
}

// credential returns the credential a command runs with. Without a policy it
// is nil, which is the daemon's own user, and a request that names a user gets
// PERMISSION_DENIED. With a policy it is the named user, or the default user
// when the request names none, and PERMISSION_DENIED when the policy does not
// allow the user the request names.
func (p *RunAsPolicy) credential(user *processv1.User) (*syscall.Credential, error) {
	if p == nil {
		if user == nil {
			return nil, nil
		}
		return nil, status.Error(codes.PermissionDenied, "user: this daemon does not run commands as another user")
	}
	if user == nil {
		// Groups stays empty, so the command gets none of the daemon's
		// supplementary groups.
		return &syscall.Credential{Uid: p.DefaultUID, Gid: p.DefaultGID}, nil
	}
	if !p.UIDs.contains(user.GetUid()) {
		return nil, status.Errorf(codes.PermissionDenied, "user: uid %d is not allowed", user.GetUid())
	}
	for _, gid := range append([]uint32{user.GetGid()}, user.GetSupplementaryGids()...) {
		if !p.GIDs.contains(gid) {
			return nil, status.Errorf(codes.PermissionDenied, "user: gid %d is not allowed", gid)
		}
	}
	// Groups is set even when empty: the kernel then clears the daemon's own
	// supplementary groups instead of passing them on to the command.
	return &syscall.Credential{Uid: user.GetUid(), Gid: user.GetGid(), Groups: user.GetSupplementaryGids()}, nil
}

// chownStdio gives the command's ends of its pipes or terminal to the user the
// command runs as. sandboxd created them as its own user, so without this a
// command that opens /dev/stdout, /dev/stderr or its terminal by path gets
// "permission denied".
func chownStdio(cmd *exec.Cmd, files ...*os.File) error {
	cred := cmd.SysProcAttr.Credential
	if cred == nil {
		return nil
	}
	for _, f := range files {
		if err := f.Chown(int(cred.Uid), -1); err != nil {
			return err
		}
	}
	return nil
}

// Bits of the Linux capabilities a RunAsPolicy needs, as numbered in
// capability.h.
const (
	capChown  = 0
	capKill   = 5
	capSetgid = 6
	capSetuid = 7
)

// checkRunAsPrivileges reports whether sandboxd holds what a RunAsPolicy needs,
// so a daemon that lacks it fails at startup instead of on the first request.
// Being root is not enough, because a root daemon can have capabilities
// dropped, so it also reads the effective set.
func checkRunAsPrivileges() error {
	procStatus, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("running commands as another user needs Linux: %w", err)
	}
	return checkRunAsCapabilities(string(procStatus))
}

// checkRunAsCapabilities is checkRunAsPrivileges for the given contents of
// /proc/self/status.
func checkRunAsCapabilities(procStatus string) error {
	sets := map[string]uint64{}
	var effectiveUID string
	for line := range strings.Lines(procStatus) {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(name, "Cap"):
			set, err := strconv.ParseUint(value, 16, 64)
			if err != nil {
				return fmt.Errorf("read /proc/self/status %s: %w", name, err)
			}
			sets[name] = set
		case name == "Uid":
			// Real, effective, saved and filesystem UID.
			if ids := strings.Fields(value); len(ids) > 1 {
				effectiveUID = ids[1]
			}
		}
	}
	// Only root loses its capabilities when it changes user. A daemon that is
	// not root would hold them through file capabilities on its binary. Its
	// child keeps them when it changes user, and keeps them again when it
	// executes that binary, so a request whose command is sandboxd would start
	// a second daemon that has them.
	if effectiveUID != "0" {
		return errors.New("running commands as another user needs sandboxd to run as root (uid 0), with at least the capabilities CAP_SETUID, CAP_SETGID, CAP_KILL and CAP_CHOWN")
	}
	var missing []string
	for _, c := range []struct {
		name string
		bit  uint
	}{
		// CAP_SETUID and CAP_SETGID change the command's user and groups.
		// CAP_KILL lets SendSignal, a cancelled stream and the shutdown sweep
		// reach the command afterwards. CAP_CHOWN is for chownStdio.
		{"CAP_SETUID", capSetuid}, {"CAP_SETGID", capSetgid}, {"CAP_KILL", capKill}, {"CAP_CHOWN", capChown},
	} {
		if sets["CapEff"]&(1<<c.bit) == 0 {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("running commands as another user needs the capabilities CAP_SETUID, CAP_SETGID, CAP_KILL and CAP_CHOWN, which root holds unless they were dropped; sandboxd lacks %s", strings.Join(missing, ", "))
	}
	// A command would inherit ambient capabilities, CAP_SETUID among them, and
	// could use them to become root.
	if sets["CapAmb"] != 0 {
		return errors.New("running commands as another user is unsafe while sandboxd holds ambient capabilities, because every command would inherit them; start sandboxd without ambient capabilities")
	}
	return nil
}

// RunAsHelperArg is the first argument of sandboxd when it runs as the run-as
// helper (see RunAsHelper) instead of as the daemon.
const RunAsHelperArg = "run-as-helper"

// The steps the run-as helper reports a failure of.
const (
	helperStepEnvironment = "environment"
	helperStepUser        = "user"
	helperStepCwdEscapes  = "cwd-escapes"
	helperStepCwdResolve  = "cwd-resolve"
	helperStepCwdEnter    = "cwd-enter"
	helperStepExec        = "exec"
)

// helperStart is a command that runs as another user, from the moment sandboxd
// started the run-as helper for it.
type helperStart struct {
	status *os.File
	// command is the path of the command the helper executes.
	command string
}

// startCommand starts cmd. A command that runs as the daemon's own user starts
// directly, and the returned helperStart is nil.
//
// A command that runs as another user does not start with that user's
// credential. sandboxd starts itself again as the run-as helper, still as
// root, and the helper becomes the user and executes the command. A user can
// signal a process as soon as it has that user's ID, and Go's fork blocks the
// daemon until the child executes. A child that changed user itself could be
// stopped at that point by the named user, which stalls the daemon. The
// daemon's child now executes as root, where only root can signal it.
func (s *ProcessServer) startCommand(cmd *exec.Cmd) (*helperStart, error) {
	credential := cmd.SysProcAttr.Credential
	if credential == nil {
		return nil, cmd.Start()
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command := cmd.Path
	groups := make([]string, len(credential.Groups))
	for i, gid := range credential.Groups {
		groups[i] = strconv.FormatUint(uint64(gid), 10)
	}
	// buildCommand left the requested working directory in cmd.Dir for the
	// helper, which enters it as the user.
	cmd.Args = append([]string{
		"sandboxd", RunAsHelperArg,
		strconv.FormatUint(uint64(credential.Uid), 10),
		strconv.FormatUint(uint64(credential.Gid), 10),
		strings.Join(groups, ","),
		s.rootDir, cmd.Dir, command,
	}, cmd.Args...)
	// Execute the running binary itself, not a path that could now name
	// another file.
	cmd.Path = "/proc/self/exe"
	cmd.Dir = ""
	cmd.SysProcAttr.Credential = nil
	// The helper starts as root, so it must not start in the command's
	// environment: a variable such as LD_PRELOAD from the request would run
	// the caller's code as root. The helper starts with no environment and
	// reads the command's from a pipe, which no other user can read.
	environment := cmd.Environ()
	cmd.Env = []string{}
	environmentRead, environmentWrite, err := os.Pipe()
	if err != nil {
		closeAll(statusRead, statusWrite)
		return nil, err
	}
	// The extra files are descriptors 3 and 4 in the helper: where it reports
	// why it could not run the command, and where it reads the environment.
	cmd.ExtraFiles = []*os.File{statusWrite, environmentRead}
	err = cmd.Start()
	// The helper holds its own copies. A read of the status ends only once
	// the helper's copy of the write end closes too.
	closeAll(statusWrite, environmentRead)
	if err != nil {
		closeAll(statusRead, environmentWrite)
		return nil, err
	}
	go func() {
		// A helper that exits early leaves this write to fail, and its status
		// already says why.
		_, _ = environmentWrite.WriteString(strings.Join(environment, "\x00"))
		_ = environmentWrite.Close()
	}()
	return &helperStart{status: statusRead, command: command}, nil
}

// result waits until the helper has executed the command or has exited, and
// returns the gRPC error for a failure the helper reported. msg describes the
// request in that error. A nil helperStart has nothing to wait for.
func (h *helperStart) result(msg string) error {
	if h == nil {
		return nil
	}
	defer h.close()
	report, _ := io.ReadAll(io.LimitReader(h.status, 64))
	if len(report) == 0 {
		// The pipe closed with nothing written: the helper executed the
		// command, or a signal killed it first, as it could kill the command.
		return nil
	}
	step, code, _ := strings.Cut(string(report), " ")
	number, _ := strconv.Atoi(code)
	errno := syscall.Errno(number)
	switch step {
	case helperStepCwdEscapes:
		return status.Errorf(codes.PermissionDenied, "cwd: access denied: path escapes sandbox root")
	case helperStepCwdResolve, helperStepCwdEnter:
		switch {
		case errno == syscall.ENOENT, errno == syscall.ENOTDIR && step == helperStepCwdResolve:
			return status.Errorf(codes.NotFound, "cwd: path does not exist: %v", errno)
		case errno == syscall.ENOTDIR:
			return status.Errorf(codes.InvalidArgument, "cwd is not a directory")
		case errno == syscall.EACCES:
			return status.Errorf(codes.PermissionDenied, "cwd: the command's user cannot enter it: %v", errno)
		}
		return status.Errorf(codes.Internal, "cwd: %v", errno)
	case helperStepExec:
		return mapCommandError(&os.PathError{Op: "exec", Path: h.command, Err: errno}, msg)
	}
	return status.Errorf(codes.Internal, "%s: cannot run the command as its user (%s): %v", msg, step, errno)
}

func (h *helperStart) close() {
	if h != nil {
		_ = h.status.Close()
	}
}

// startCommandAndWait starts cmd and returns once the command itself runs. For
// a command that runs as another user, that is once the run-as helper has
// executed it. The returned error is a gRPC status.
func (s *ProcessServer) startCommandAndWait(ctx context.Context, cmd *exec.Cmd, msg string) error {
	helper, err := s.startCommand(cmd)
	if err != nil {
		return mapCommandError(err, msg)
	}
	if helper == nil {
		return nil
	}
	// The helper has the named user's ID before it executes the command, so
	// that user can stop it there. Do not wait for it longer than the caller
	// waits for the request.
	stop := context.AfterFunc(ctx, func() { s.killProcessTree(context.Background(), cmd) })
	err = helper.result(msg)
	stop()
	if err != nil {
		_ = cmd.Wait()
	}
	return err
}
