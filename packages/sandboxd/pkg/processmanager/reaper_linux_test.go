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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestLinuxSubreaperOrphanReaped verifies that when running as a child subreaper
// (mirroring PID 1 init process semantics on Linux), orphaned grandchildren
// re-parented to this process are reaped by Reaper and do not accumulate as
// defunct zombies.
func TestLinuxSubreaperOrphanReaped(t *testing.T) {
	// Enable subreaper status for this process so orphaned grandchildren reparent to us.
	err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	require.NoError(t, err, "prctl(PR_SET_CHILD_SUBREAPER, 1) must succeed")
	defer func() {
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
	}()

	reaper := StartReaper(logr.Discard())
	defer reaper.Stop()

	initialReaped := reaper.ReapedOrphans()

	// Child A starts Grandchild B in background and exits immediately.
	// Grandchild B sleeps briefly and exits.
	script := `sh -c 'sleep 0.1; exit 0' & echo $!; exit 0`
	out, err := exec.Command("sh", "-c", script).Output()
	require.NoError(t, err)

	grandchildPIDStr := strings.TrimSpace(string(out))
	grandchildPID, err := strconv.Atoi(grandchildPIDStr)
	require.NoError(t, err)
	require.Positive(t, grandchildPID)

	// Verify that the grandchild was reaped and no longer exists in the process table.
	require.Eventually(t, func() bool {
		// 1. Check reaper counter
		if reaper.ReapedOrphans() <= initialReaped {
			return false
		}
		// 2. kill(pid, 0) should return ESRCH once the zombie is reaped
		kErr := syscall.Kill(grandchildPID, 0)
		if !errors.Is(kErr, syscall.ESRCH) {
			return false
		}
		// 3. /proc/<pid> should no longer exist
		procPath := fmt.Sprintf("/proc/%d", grandchildPID)
		if _, statErr := os.Stat(procPath); !os.IsNotExist(statErr) {
			return false
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "orphaned grandchild %d must be reaped by subreaper", grandchildPID)
}
