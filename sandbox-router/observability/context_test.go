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

package observability

import (
	"context"
	"testing"
)

func TestLabelsForRequest_AllocatesWhenNoneAttached(t *testing.T) {
	ctx, labels := LabelsForRequest(context.Background())
	if labels == nil {
		t.Fatal("expected a non-nil *Labels")
	}
	if got := LabelsFromContext(ctx); got != labels {
		t.Fatalf("LabelsFromContext(ctx) = %p, want the same pointer %p returned by LabelsForRequest", got, labels)
	}
}

// TestLabelsForRequest_ReusesOuterLabels is the regression test for the bug
// this helper exists to prevent: if every middleware in the chain
// unconditionally allocated its own *Labels, an outer layer (e.g.
// TracingMiddleware) would end up with a *Labels the proxy handler never
// sees or mutates — only the innermost allocation would ever be populated.
func TestLabelsForRequest_ReusesOuterLabels(t *testing.T) {
	outerCtx, outerLabels := LabelsForRequest(context.Background())

	innerCtx, innerLabels := LabelsForRequest(outerCtx)
	if innerLabels != outerLabels {
		t.Fatalf("inner call allocated a new *Labels (%p) instead of reusing the outer one (%p)", innerLabels, outerLabels)
	}
	if innerCtx != outerCtx {
		t.Fatal("expected the context to be returned unchanged when Labels was already attached")
	}

	// Whatever the innermost handler sets must be visible through the
	// outer reference — same pointer, same struct.
	innerLabels.SandboxID = "my-box"
	innerLabels.SandboxNamespace = "test"
	if outerLabels.SandboxID != "my-box" || outerLabels.SandboxNamespace != "test" {
		t.Fatalf("mutation via inner reference not visible via outer: %+v", *outerLabels)
	}
}
