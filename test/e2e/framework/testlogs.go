// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package framework

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logCapturingT wraps a T and captures all log output to a file in addition
// to the normal test output. This allows per-test log files with timing info.
type logCapturingT struct {
	T
	file      *os.File
	mu        sync.Mutex
	startTime time.Time
}

// newLogCapturingT creates a new logCapturingT that writes logs to a file in artifactsDir.
func newLogCapturingT(t T, artifactsDir string) *logCapturingT {
	logFile := filepath.Join(artifactsDir, "test.log")
	f, err := os.Create(logFile)
	if err != nil {
		t.Logf("warning: failed to create test log file %s: %v", logFile, err)
		return &logCapturingT{T: t, startTime: time.Now()}
	}

	t.Cleanup(func() {
		f.Close()
	})

	lc := &logCapturingT{
		T:         t,
		file:      f,
		startTime: time.Now(),
	}
	lc.writeToFile("=== Test started: %s @%v ===\n", t.Name(), lc.startTime.UTC().Format("2006-01-02T15:04:05.000"))
	return lc
}

func (lc *logCapturingT) writeToFile(format string, args ...any) {
	if lc.file == nil {
		return
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()

	elapsed := time.Since(lc.startTime)
	timestamp := fmt.Sprintf("[%10.3fs] ", elapsed.Seconds())
	fmt.Fprintf(lc.file, timestamp+format, args...)
}

func (lc *logCapturingT) Log(args ...any) {
	lc.T.Helper()
	lc.T.Log(args...)
	lc.writeToFile("%s\n", fmt.Sprint(args...))
}

func (lc *logCapturingT) Logf(format string, args ...any) {
	lc.T.Helper()
	lc.T.Logf(format, args...)
	lc.writeToFile(format+"\n", args...)
}

func (lc *logCapturingT) Error(args ...any) {
	lc.T.Helper()
	lc.T.Error(args...)
	lc.writeToFile("ERROR: %s\n", fmt.Sprint(args...))
}

func (lc *logCapturingT) Errorf(format string, args ...any) {
	lc.T.Helper()
	lc.T.Errorf(format, args...)
	lc.writeToFile("ERROR: "+format+"\n", args...)
}

func (lc *logCapturingT) Fatal(args ...any) {
	lc.T.Helper()
	lc.writeToFile("FATAL: %s\n", fmt.Sprint(args...))
	lc.T.Fatal(args...)
}

func (lc *logCapturingT) Fatalf(format string, args ...any) {
	lc.T.Helper()
	lc.writeToFile("FATAL: "+format+"\n", args...)
	lc.T.Fatalf(format, args...)
}
