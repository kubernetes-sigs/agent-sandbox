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
	"testing"
)

func TestProcessRegistry(t *testing.T) {
	registry := NewProcessRegistry()

	pid1 := registry.NextPID()
	pid2 := registry.NextPID()

	if pid1 != 1 || pid2 != 2 {
		t.Errorf("expected PIDs 1 and 2, got %d and %d", pid1, pid2)
	}

	p1 := &ManagedProcess{ID: pid1}
	registry.Register(p1)

	got, ok := registry.Get(pid1)
	if !ok || got.ID != pid1 {
		t.Errorf("failed to get registered process %d", pid1)
	}

	active := registry.ListActive()
	if len(active) != 1 {
		t.Errorf("expected 1 active process, got %d", len(active))
	}

	registry.Remove(pid1)
	_, ok = registry.Get(pid1)
	if ok {
		t.Errorf("expected process %d to be removed", pid1)
	}
}
