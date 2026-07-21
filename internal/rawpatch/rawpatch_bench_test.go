// Copyright 2025 The Kubernetes Authors.
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

package rawpatch

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// The representative mutation both benchmarks build: the two-annotation
// metadata patch the SandboxClaim controller persists on first observation
// (first-observed timestamp + trace context).
const (
	benchObsKey   = "agents.x-k8s.io/controller-first-observed-at"
	benchObsVal   = "2026-07-19T15:27:56.851234567Z"
	benchTraceKey = "opentelemetry.io/trace-context"
	benchTraceVal = `{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`
)

// benchClaim returns a realistically sized SandboxClaim: populated metadata,
// spec with warm pool ref and lifecycle, and a bound status. This is the
// object the legacy path DeepCopies and serializes twice per patch.
func benchClaim() *extensionsv1beta1.SandboxClaim {
	shutdown := metav1.NewTime(time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC))
	created := metav1.NewTime(time.Date(2026, 7, 19, 15, 27, 55, 0, time.UTC))
	return &extensionsv1beta1.SandboxClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions.agents.x-k8s.io/v1beta1",
			Kind:       "SandboxClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bench-claim-042",
			Namespace:         "agents-bench",
			UID:               "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
			ResourceVersion:   "123456789",
			Generation:        1,
			CreationTimestamp: created,
			Labels: map[string]string{
				"agents.x-k8s.io/created-by": "bench-suite",
				"team.example.com/owner":     "platform",
			},
			Annotations: map[string]string{
				"agents.x-k8s.io/sandbox-name":         "warm-sb-042",
				"agents.x-k8s.io/webhook-observed-at":  "2026-07-19T15:27:55.998Z",
				"kubectl.kubernetes.io/last-applied-c": "{}",
			},
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "bench-pool"},
			Lifecycle: &extensionsv1beta1.Lifecycle{
				ShutdownTime:   &shutdown,
				ShutdownPolicy: extensionsv1beta1.ShutdownPolicyRetain,
			},
		},
		Status: extensionsv1beta1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "SandboxReady",
					Message:            "Sandbox is ready",
					LastTransitionTime: created,
					ObservedGeneration: 1,
				},
			},
			SandboxStatus: extensionsv1beta1.SandboxStatus{
				Name:   "warm-sb-042",
				PodIPs: []string{"10.12.34.56"},
			},
		},
	}
}

// BenchmarkRawPatch measures building the two-annotation metadata patch with
// rawpatch: marshal only the intended keys, O(patch).
func BenchmarkRawPatch(b *testing.B) {
	claim := benchClaim()
	b.ReportAllocs()
	for b.Loop() {
		p, err := Annotations(map[string]string{
			benchObsKey:   benchObsVal,
			benchTraceKey: benchTraceVal,
		})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := p.Data(claim); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMergeFromDiff measures the legacy pattern for the exact same
// mutation: DeepCopy the object for a base snapshot, mutate, and let
// client.MergeFrom serialize both full documents and diff them, O(object).
func BenchmarkMergeFromDiff(b *testing.B) {
	claim := benchClaim()
	b.ReportAllocs()
	for b.Loop() {
		patch := client.MergeFrom(claim.DeepCopy())
		claim.Annotations[benchObsKey] = benchObsVal
		claim.Annotations[benchTraceKey] = benchTraceVal
		if _, err := patch.Data(claim); err != nil {
			b.Fatal(err)
		}
		// Reset so every iteration diffs the same base/modified pair. Two map
		// deletes are noise next to the DeepCopy + double marshal above.
		delete(claim.Annotations, benchObsKey)
		delete(claim.Annotations, benchTraceKey)
	}
}

// TestBenchPathsAgreeOnWire pins that the two benchmarked paths produce the
// same bytes for the benchmark mutation, so the comparison is apples to
// apples.
func TestBenchPathsAgreeOnWire(t *testing.T) {
	claim := benchClaim()

	legacyBase := client.MergeFrom(claim.DeepCopy())
	claim.Annotations[benchObsKey] = benchObsVal
	claim.Annotations[benchTraceKey] = benchTraceVal
	legacyData, err := legacyBase.Data(claim)
	if err != nil {
		t.Fatalf("MergeFrom Data() error: %v", err)
	}

	raw, err := Annotations(map[string]string{
		benchObsKey:   benchObsVal,
		benchTraceKey: benchTraceVal,
	})
	if err != nil {
		t.Fatalf("Annotations() error: %v", err)
	}
	rawData, err := raw.Data(claim)
	if err != nil {
		t.Fatalf("raw Data() error: %v", err)
	}

	if string(rawData) != string(legacyData) {
		t.Errorf("wire payloads differ:\n raw:   %s\n merge: %s", rawData, legacyData)
	}
}
