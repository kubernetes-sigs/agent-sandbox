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

import "testing"

func TestLimitedWriter(t *testing.T) {
	w := NewLimitedWriter(8)
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Errorf("Error writing to LimitedWriter: %v", err)
	}
	if n != 5 {
		t.Errorf("Expected %d bytes written, got %d", 5, n)
	}
	if w.String() != "hello" {
		t.Errorf("Expected %q, got %q", "hello", w.String())
	}
	if w.Truncated() {
		t.Errorf("Expected not truncated, got truncated")
	}

	n, err = w.Write([]byte("world"))
	if err != nil {
		t.Errorf("Error writing to LimitedWriter: %v", err)
	}
	if n != 5 {
		t.Errorf("Expected %d bytes written, got %d", 5, n)
	}
	if w.String() != "hellowor" {
		t.Errorf("Expected %q, got %q", "hellowor", w.String())
	}
	if !w.Truncated() {
		t.Errorf("Expected truncated, got not truncated")
	}
}
