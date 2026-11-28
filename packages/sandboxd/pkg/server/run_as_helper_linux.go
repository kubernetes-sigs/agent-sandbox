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

//go:build linux

package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
)

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS from linux/prctl.h. Package syscall
// does not define it on every architecture.
const prSetNoNewPrivs = 38

// The descriptors of the exec.Cmd.ExtraFiles that startCommand passes. The
// helper reports on the first why it could not run the command, and reads the
// command's environment from the second as entries separated by NUL bytes.
const (
	helperStatusFD      = 3
	helperEnvironmentFD = 4
)

// RunAsHelper is what sandboxd does when its first argument is
// RunAsHelperArg. The daemon starts it as root for every command that runs as
// another user (see startCommand). The helper becomes that user, gives up
// every way back to the daemon's privileges, enters the working directory and
// executes the command in its own place. It reports a failure on
// helperStatusFD and never returns.
//
// The helper starts with an empty environment, because it starts as root and
// the command's environment comes from the request.
//
// args are the user ID, the group ID, the supplementary group IDs separated
// by commas, the sandbox root, the requested working directory, the path of
// the command, and the command's arguments starting with its name.
func RunAsHelper(args []string) {
	statusPipe := os.NewFile(helperStatusFD, "status")
	// The pipe must close when the command executes. The daemon reads that as
	// success, and the command must not inherit the pipe.
	syscall.CloseOnExec(helperStatusFD)
	fail := func(step string, err error) {
		var errno syscall.Errno
		errors.As(err, &errno)
		_, _ = fmt.Fprintf(statusPipe, "%s %d", step, int(errno))
		os.Exit(1)
	}

	// Only the daemon, which is root, may use the helper. Anyone else who
	// runs the binary this way gets nothing from it, whatever file
	// capabilities the binary carries.
	if len(args) < 7 || os.Geteuid() != 0 {
		fail(helperStepUser, syscall.EPERM)
	}
	uid, uidErr := strconv.ParseUint(args[0], 10, 32)
	gid, gidErr := strconv.ParseUint(args[1], 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		fail(helperStepUser, syscall.EINVAL)
	}
	groups := []int{}
	for value := range strings.SplitSeq(args[2], ",") {
		if value == "" {
			continue
		}
		group, err := strconv.ParseUint(value, 10, 32)
		if err != nil || group == 0 {
			fail(helperStepUser, syscall.EINVAL)
		}
		groups = append(groups, int(group))
	}
	root, cwd, path, argv := args[3], args[4], args[5], args[6:]

	environmentPipe := os.NewFile(helperEnvironmentFD, "environment")
	encoded, err := io.ReadAll(environmentPipe)
	_ = environmentPipe.Close()
	if err != nil {
		fail(helperStepEnvironment, err)
	}
	var environment []string
	for entry := range strings.SplitSeq(string(encoded), "\x00") {
		// PWD is set below, once the working directory is known.
		if entry != "" && !strings.HasPrefix(entry, "PWD=") {
			environment = append(environment, entry)
		}
	}

	// no_new_privs and capabilities belong to a thread, so the calls below
	// and the exec must run on the same one.
	runtime.LockOSThread()
	// An empty list still clears the daemon's own supplementary groups.
	if err := syscall.Setgroups(groups); err != nil {
		fail(helperStepUser, err)
	}
	if err := syscall.Setgid(int(gid)); err != nil {
		fail(helperStepUser, err)
	}
	if err := syscall.Setuid(int(uid)); err != nil {
		fail(helperStepUser, err)
	}
	// With no_new_privs, a setuid program or a file with capabilities gives
	// the command and its children nothing. Without it, one such program in
	// the image would hand a named user root with the daemon's capabilities.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		fail(helperStepUser, errno)
	}
	// Root loses its capabilities when it changes user. Run nothing if any
	// survived, for example because securebits keep them.
	if !holdsNoCapabilities() {
		fail(helperStepUser, syscall.EPERM)
	}

	// Resolve and enter the working directory as the user. The daemon may
	// lack the privilege to look inside a directory that only its owner can
	// enter, and the user's own permissions are the ones that should apply.
	dir, err := pathutil.SanitizePath(root, cwd)
	if errors.Is(err, pathutil.ErrPathEscapes) {
		fail(helperStepCwdEscapes, err)
	}
	if err != nil {
		fail(helperStepCwdResolve, err)
	}
	if err := os.Chdir(dir); err != nil {
		fail(helperStepCwdEnter, err)
	}
	fail(helperStepExec, syscall.Exec(path, argv, append(environment, "PWD="+dir)))
}

// holdsNoCapabilities reports whether the calling thread's permitted,
// effective and ambient capability sets are all empty.
func holdsNoCapabilities() bool {
	threadStatus, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/status", syscall.Gettid()))
	if err != nil {
		return false
	}
	found := 0
	for line := range strings.Lines(string(threadStatus)) {
		name, value, _ := strings.Cut(line, ":")
		if name != "CapPrm" && name != "CapEff" && name != "CapAmb" {
			continue
		}
		if set, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64); err != nil || set != 0 {
			return false
		}
		found++
	}
	return found == 3
}
