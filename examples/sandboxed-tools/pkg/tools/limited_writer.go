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

package tools

import (
	"bytes"
	"sync"
)

// LimitedWriter is a Writer that limits the amount of data that can be written to it.
// It is also safe for concurrent use by multiple goroutines (e.g. multiple writers).
type LimitedWriter struct {
	mutex     sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (w *LimitedWriter) Write(p []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if w.truncated {
		return len(p), nil
	}
	if w.limit != 0 {
		remaining := w.limit - w.buffer.Len()
		if remaining <= 0 {
			w.truncated = true
			return len(p), nil
		}
		if len(p) > remaining {
			_, _ = w.buffer.Write(p[:remaining])
			w.truncated = true
			return len(p), nil
		}
	}
	return w.buffer.Write(p)
}

func (w *LimitedWriter) String() string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.buffer.String()
}

func (w *LimitedWriter) Truncated() bool {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.truncated
}

func (w *LimitedWriter) Len() int {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.buffer.Len()
}

// NewLimitedWriter creates a new LimitedWriter with the given limit.
func NewLimitedWriter(limit int) *LimitedWriter {
	return &LimitedWriter{limit: limit}
}
