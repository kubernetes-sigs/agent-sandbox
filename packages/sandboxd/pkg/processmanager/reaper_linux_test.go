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

package processmanager

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// becomeSubreaper makes the test process inherit orphaned descendants, as
// sandboxd does when it runs as PID 1.
func becomeSubreaper(t *testing.T) {
	t.Helper()
	const prSetChildSubreaper = 36
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	require.Zero(t, errno)
}

// startOrphan runs a tracked shell that leaves a short-lived background process
// behind, and returns that process's PID.
func startOrphan(t *testing.T, r *ProcessRegistry) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 0.2 >/dev/null 2>&1 & echo $!")
	var out bytes.Buffer
	cmd.Stdout = &out
	require.NoError(t, r.StartChild(cmd, cmd.Start))
	require.NoError(t, r.WaitChild(cmd))
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	require.NoError(t, err)
	return pid
}

func TestReapOrphansCollectsUntrackedChildren(t *testing.T) {
	becomeSubreaper(t)
	r := NewProcessRegistry()

	orphan := startOrphan(t, r)

	require.Eventually(t, func() bool {
		r.ReapOrphans()
		return errors.Is(syscall.Kill(orphan, 0), syscall.ESRCH)
	}, 5*time.Second, 20*time.Millisecond)
}

func TestRunReaperCollectsOrphansOnSIGCHLD(t *testing.T) {
	becomeSubreaper(t)
	r := NewProcessRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunReaper(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	orphan := startOrphan(t, r)

	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(orphan, 0), syscall.ESRCH)
	}, 5*time.Second, 20*time.Millisecond)
}

func TestReapOrphansLeavesTrackedChildrenForWait(t *testing.T) {
	r := NewProcessRegistry()

	cmd := exec.Command("sh", "-c", "exit 7")
	require.NoError(t, r.StartChild(cmd, cmd.Start))
	waitForZombie(t, cmd.Process.Pid)

	require.Zero(t, r.ReapOrphans())

	var exitErr *exec.ExitError
	require.ErrorAs(t, r.WaitChild(cmd), &exitErr)
	require.Equal(t, 7, exitErr.ExitCode())
}

func TestWaitChildForgetsThePID(t *testing.T) {
	r := NewProcessRegistry()

	cmd := exec.Command("true")
	require.NoError(t, r.StartChild(cmd, cmd.Start))
	require.NoError(t, r.WaitChild(cmd))

	require.Empty(t, r.children)
}

// waitForZombie blocks until pid has exited but not been reaped.
func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	var info [128]byte // siginfo_t
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno != syscall.EINTR {
			require.Zero(t, errno)
			return
		}
	}
}
