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

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	fakeBinOnce sync.Once
	fakeBinPath string
	fakeBinErr  error
)

// fakeBin builds internal/fakeexec once per test binary and returns its path.
// The binary lives in a process-lifetime temp dir (not t.TempDir, which the
// first test would clean up before later tests run).
func fakeBin(t *testing.T) string {
	t.Helper()
	fakeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeexec")
		if err != nil {
			fakeBinErr = err
			return
		}
		bin := filepath.Join(dir, "fakeexec")
		fakeBinErr = exec.Command("go", "build", "-o", bin, "../fakeexec").Run()
		if fakeBinErr == nil {
			fakeBinPath = bin
		}
	})
	if fakeBinErr != nil {
		t.Fatalf("building fakeexec: %v", fakeBinErr)
	}
	return fakeBinPath
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
