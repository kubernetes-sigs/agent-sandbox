//go:build linux

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
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPutStaysInsideRootWhileDirectoryIsSwapped(t *testing.T) {
	base := t.TempDir()
	dir, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	writeFiles(t, base, map[string]string{"root/shared/real/keep": "k"})
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "shared", "link")))
	handler := NewRESTServer(dir, "SANDBOX_", logr.Discard()).Handler()
	dirPath, linkPath := filepath.Join(dir, "shared", "real"), filepath.Join(dir, "shared", "link")
	exchange := func() error {
		return unix.Renameat2(unix.AT_FDCWD, dirPath, unix.AT_FDCWD, linkPath, unix.RENAME_EXCHANGE)
	}
	if err := exchange(); errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		t.Skipf("RENAME_EXCHANGE is not supported here: %v", err)
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, exchange())

	// A process in the sandbox exchanges a directory and a symlink out of the
	// root in a loop, so some request resolves its path just before the swap.
	var stop atomic.Bool
	var swaps int
	var workerErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() && workerErr == nil {
			if workerErr = exchange(); workerErr == nil {
				swaps++
			}
		}
	}()
	for i, deadline := 0, time.Now().Add(time.Second); time.Now().Before(deadline); i++ {
		doRequest(handler, http.MethodPut, fmt.Sprintf("/v1/files/shared/real/f-%d", i), []byte("x"), nil)
	}
	stop.Store(true)
	<-done

	require.NoError(t, workerErr)
	require.Positive(t, swaps)
	require.Empty(t, readTree(t, outside), "a write left the sandbox root")
}
