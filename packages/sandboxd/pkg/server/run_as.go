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
	"errors"
	"fmt"
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
// ProcessConfig.user. A nil policy refuses every named user.
type RunAsPolicy struct {
	UIDs IDRange
	GIDs IDRange
}

// credential returns the credential a command runs with: nil when the request
// names no user, and PERMISSION_DENIED when the policy does not allow the user
// it names.
func (p *RunAsPolicy) credential(user *processv1.User) (*syscall.Credential, error) {
	if user == nil {
		return nil, nil
	}
	if p == nil {
		return nil, status.Error(codes.PermissionDenied, "user: this daemon does not run commands as another user")
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
// It reads the effective set instead of checking for uid 0, because a root
// daemon can have capabilities dropped and a non-root daemon can hold them
// through file capabilities.
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
	for line := range strings.Lines(procStatus) {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.HasPrefix(name, "Cap") {
			set, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil {
				return fmt.Errorf("read /proc/self/status %s: %w", name, err)
			}
			sets[name] = set
		}
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
		return errors.New("running commands as another user is unsafe while sandboxd holds ambient capabilities, because every command would inherit them; run sandboxd as root or grant its binary file capabilities instead")
	}
	return nil
}
