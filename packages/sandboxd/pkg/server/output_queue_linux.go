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
	"os"

	"golang.org/x/sys/unix"
)

// queuedOutputBytes returns how many bytes are waiting to be read from file.
// TIOCINQ (FIONREAD) reports this for a pipe and for a PTY master. If the
// call fails, the pipe capacity stands in. SyscallConn is used instead of Fd
// because Fd puts the file in blocking mode, and then closing it would no
// longer unblock a pending Read.
func queuedOutputBytes(file *os.File) int64 {
	queued := int64(defaultPipeCapacity)
	conn, err := file.SyscallConn()
	if err != nil {
		return queued
	}
	_ = conn.Control(func(fd uintptr) {
		if n, err := unix.IoctlGetInt(int(fd), unix.TIOCINQ); err == nil {
			queued = int64(n)
		}
	})
	return queued
}
