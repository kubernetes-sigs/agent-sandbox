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

package toolsets

import (
	"testing"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

func TestGet(t *testing.T) {
	for _, name := range Names() {
		toolset, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if got := toolset.Name(); got != name {
			t.Errorf("Get(%q).Name() = %q", name, got)
		}
		if toolset.SystemPrompt() == "" {
			t.Errorf("toolset %q: empty system prompt", name)
		}
		registry := tools.NewRegistry()
		toolset.RegisterTools(registry)
		if len(registry.All()) == 0 {
			t.Errorf("toolset %q: no tools registered", name)
		}
	}
}

func TestGetDefaultsToBasic(t *testing.T) {
	toolset, err := Get("")
	if err != nil {
		t.Fatalf("Get(\"\"): %v", err)
	}
	if toolset.Name() != "basic" {
		t.Errorf("default toolset = %q, want basic", toolset.Name())
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := Get("no-such-toolset"); err == nil {
		t.Fatal("expected error for unknown toolset")
	}
}
