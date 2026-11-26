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

package processmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapInterval is how often RunReaper rescans without a SIGCHLD. A scan can
// skip a zombie when its PID is briefly shared with a recorded child, or when
// the kernel omits an entry while a sibling exits mid-read.
const reapInterval = 10 * time.Second

// StartChild runs start, which must start cmd, and records the child's OS PID
// as one that sandboxd waits on itself. The registry holds its lock until the
// PID is recorded, so ReapOrphans never sees the child before then.
func (r *ProcessRegistry) StartChild(cmd *exec.Cmd, start func() error) error {
	r.childMu.Lock()
	defer r.childMu.Unlock()
	if err := start(); err != nil {
		return err
	}
	r.children[cmd.Process.Pid]++
	return nil
}

// WaitChild waits for a child recorded by StartChild and then forgets its PID.
func (r *ProcessRegistry) WaitChild(cmd *exec.Cmd) error {
	err := cmd.Wait()
	r.childMu.Lock()
	defer r.childMu.Unlock()
	// The count tolerates the kernel reusing the PID for a new child before this runs.
	if pid := cmd.Process.Pid; r.children[pid] > 1 {
		r.children[pid]--
	} else {
		delete(r.children, pid)
	}
	return err
}

// ReapOrphans collects exited children of this process that StartChild did not
// record, such as grandchildren re-parented to sandboxd when it runs as PID 1.
// It returns how many it collected. It never touches a recorded child, so
// cmd.Wait keeps the real exit status and the PID stays reserved until then.
func (r *ProcessRegistry) ReapOrphans() int {
	r.childMu.Lock()
	defer r.childMu.Unlock()
	reaped := 0
	for _, pid := range ReadProcChildren(context.Background(), os.Getpid()) {
		if r.children[pid] > 0 {
			continue
		}
		if wpid, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err == nil && wpid == pid {
			reaped++
		}
	}
	return reaped
}

// RunReaper reaps orphans each time the process receives SIGCHLD, until ctx is
// done. Run it only when sandboxd is PID 1, because only then do orphans reach it.
func (r *ProcessRegistry) RunReaper(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGCHLD)
	defer signal.Stop(sigCh)
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	r.ReapOrphans()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			r.ReapOrphans()
		case <-ticker.C:
			r.ReapOrphans()
		}
	}
}

// ProcChildrenAvailable reports whether this kernel exposes the children file
// that ReadProcChildren relies on. Without it ReapOrphans finds nothing.
func ProcChildrenAvailable() bool {
	_, err := os.Stat(fmt.Sprintf("/proc/self/task/%d/children", os.Getpid()))
	return err == nil
}

// ReadProcChildren returns the direct children of pid by iterating every
// thread's /proc/<pid>/task/<tid>/children (a Linux-specific interface
// enabled by CONFIG_PROC_CHILDREN). Children forked by a non-main thread
// only appear under that thread's TID, so scanning only the main PID
// would miss grandchildren of multithreaded parents. Returns nil when the
// directory is unavailable.
func ReadProcChildren(ctx context.Context, pid int) []int {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{})
	var children []int
	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%s/children", taskDir, entry.Name()))
		if err != nil {
			continue
		}
		for f := range strings.FieldsSeq(string(data)) {
			if cpid, err := strconv.Atoi(f); err == nil && cpid > 0 {
				if _, ok := seen[cpid]; !ok {
					seen[cpid] = struct{}{}
					children = append(children, cpid)
				}
			}
		}
	}
	return children
}
