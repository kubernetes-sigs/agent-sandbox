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

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// staticTraceTracer wraps the no-op instrumenter but returns a fixed trace
// context, so initializeAnnotations deterministically stamps both keys.
type staticTraceTracer struct {
	asmetrics.Instrumenter
	traceContext string
}

func (s staticTraceTracer) GetTraceContext(context.Context) string { return s.traceContext }

// TestInitializeAnnotationsRawPayload pins the exact bytes the rawpatch
// rewrite of initializeAnnotations puts on the wire, and proves they are
// identical to what the historical DeepCopy+MergeFrom pattern computed for
// the same mutation.
func TestInitializeAnnotationsRawPayload(t *testing.T) {
	scheme := newScheme(t)
	observed := time.Date(2026, 7, 19, 15, 27, 56, 850000000, time.UTC)
	const traceContext = "00-abc-def-01"
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				"pre-existing/anno": "kept",
			},
		},
	}

	var captured []byte
	var capturedType types.PatchType
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				captured = data
				capturedType = patch.Type()
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &SandboxClaimReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Tracer: staticTraceTracer{Instrumenter: asmetrics.NewNoOp(), traceContext: traceContext},
	}
	// Seed the observed-time map so the stamped timestamp is deterministic.
	r.observedTimes.Store(
		types.NamespacedName{Name: "test-claim", Namespace: "default"},
		observedTimeEntry{timestamp: observed, uid: claim.UID},
	)

	live := claim.DeepCopy()
	if err := r.initializeAnnotations(context.Background(), live); err != nil {
		t.Fatalf("initializeAnnotations failed: %v", err)
	}

	if capturedType != types.MergePatchType {
		t.Fatalf("patch type = %v, want %v", capturedType, types.MergePatchType)
	}

	// Byte-exact expectation: only the stamped keys, in sorted key order
	// ("agents.x-k8s.io/..." sorts before "opentelemetry.io/..."), nothing else.
	want := `{"metadata":{"annotations":{"` + asmetrics.ObservabilityAnnotation +
		`":"2026-07-19T15:27:56.85Z","` + asmetrics.TraceContextAnnotation +
		`":"` + traceContext + `"}}}`
	if string(captured) != want {
		t.Errorf("payload mismatch:\n got: %s\nwant: %s", captured, want)
	}

	// Equivalence with the legacy pattern (DeepCopy base, mutate, MergeFrom
	// diff): identical bytes on the wire.
	legacyModified := claim.DeepCopy()
	legacyModified.Annotations[asmetrics.ObservabilityAnnotation] = observed.Format(time.RFC3339Nano)
	legacyModified.Annotations[asmetrics.TraceContextAnnotation] = traceContext
	legacyData, err := client.MergeFrom(claim.DeepCopy()).Data(legacyModified)
	if err != nil {
		t.Fatalf("legacy MergeFrom Data() failed: %v", err)
	}
	if string(captured) != string(legacyData) {
		t.Errorf("raw payload differs from legacy MergeFrom payload:\n raw:    %s\n legacy: %s", captured, legacyData)
	}

	// The annotations must actually be persisted (and pre-existing ones kept).
	got := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Annotations[asmetrics.ObservabilityAnnotation] != "2026-07-19T15:27:56.85Z" {
		t.Errorf("observability annotation = %q, want stamped timestamp", got.Annotations[asmetrics.ObservabilityAnnotation])
	}
	if got.Annotations[asmetrics.TraceContextAnnotation] != traceContext {
		t.Errorf("trace context annotation = %q, want %q", got.Annotations[asmetrics.TraceContextAnnotation], traceContext)
	}
	if got.Annotations["pre-existing/anno"] != "kept" {
		t.Errorf("pre-existing annotation lost: %v", got.Annotations)
	}

	// Idempotence short-circuit: annotations already present make no API call.
	captured = nil
	if err := r.initializeAnnotations(context.Background(), live); err != nil {
		t.Fatalf("second initializeAnnotations failed: %v", err)
	}
	if captured != nil {
		t.Errorf("expected no patch when annotations already present, got %s", captured)
	}
}

// TestInitializeAnnotationsDisabledStampsInMemoryOnly verifies the
// --disable-claim-observability-annotations behavior at the call site: no API
// write happens, but the in-memory object is still stamped so same-process
// metrics and trace propagation keep working.
func TestInitializeAnnotationsDisabledStampsInMemoryOnly(t *testing.T) {
	scheme := newScheme(t)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
	}

	patches := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &SandboxClaimReconciler{
		Client:                          fakeClient,
		Scheme:                          scheme,
		Tracer:                          staticTraceTracer{Instrumenter: asmetrics.NewNoOp(), traceContext: "00-abc-def-01"},
		DisableObservabilityAnnotations: true,
	}

	live := claim.DeepCopy()
	if err := r.initializeAnnotations(context.Background(), live); err != nil {
		t.Fatalf("initializeAnnotations failed: %v", err)
	}

	if patches != 0 {
		t.Errorf("expected 0 patches with the flag enabled, got %d", patches)
	}
	if live.Annotations[asmetrics.ObservabilityAnnotation] == "" {
		t.Error("observability annotation should be stamped in-memory")
	}
	if live.Annotations[asmetrics.TraceContextAnnotation] != "00-abc-def-01" {
		t.Error("trace context annotation should be stamped in-memory")
	}

	got := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Annotations[asmetrics.ObservabilityAnnotation] != "" {
		t.Errorf("observability annotation should not be persisted, got %q", got.Annotations[asmetrics.ObservabilityAnnotation])
	}
}

// TestInitializeSandboxLaunchTypeLabelRawPayload pins the exact single-label
// merge-patch payload and its equivalence with the legacy MergeFrom bytes.
func TestInitializeSandboxLaunchTypeLabelRawPayload(t *testing.T) {
	scheme := newScheme(t)
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       "warm-sb-uid",
			Labels:    map[string]string{"existing": "label"},
		},
	}

	var captured []byte
	var capturedType types.PatchType
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				captured = data
				capturedType = patch.Type()
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &SandboxClaimReconciler{Client: fakeClient, Scheme: scheme}
	live := sandbox.DeepCopy()
	if err := r.initializeSandboxLaunchTypeLabel(context.Background(), live, sandboxv1beta1.SandboxLaunchTypeWarm); err != nil {
		t.Fatalf("initializeSandboxLaunchTypeLabel failed: %v", err)
	}

	if capturedType != types.MergePatchType {
		t.Fatalf("patch type = %v, want %v", capturedType, types.MergePatchType)
	}
	want := `{"metadata":{"labels":{"` + sandboxv1beta1.SandboxLaunchTypeLabel + `":"` + sandboxv1beta1.SandboxLaunchTypeWarm + `"}}}`
	if string(captured) != want {
		t.Errorf("payload mismatch:\n got: %s\nwant: %s", captured, want)
	}

	// Legacy equivalence.
	legacy := sandbox.DeepCopy()
	base := legacy.DeepCopy()
	legacy.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	legacyData, err := client.MergeFrom(base).Data(legacy)
	if err != nil {
		t.Fatalf("legacy MergeFrom Data() failed: %v", err)
	}
	if string(captured) != string(legacyData) {
		t.Errorf("raw payload differs from legacy MergeFrom payload:\n raw:    %s\n legacy: %s", captured, legacyData)
	}

	// Idempotence short-circuit: a sandbox that already has the label makes
	// no API call at all.
	captured = nil
	if err := r.initializeSandboxLaunchTypeLabel(context.Background(), live, sandboxv1beta1.SandboxLaunchTypeWarm); err != nil {
		t.Fatalf("second initializeSandboxLaunchTypeLabel failed: %v", err)
	}
	if captured != nil {
		t.Errorf("expected no patch when label already present, got %s", captured)
	}
}

// warmAdoptionFixtures returns the objects for a warm-pool adoption scenario:
// a claim, its warm pool, the pool's template, and a Ready warm sandbox owned
// by the pool.
func warmAdoptionFixtures() (*extensionsv1beta1.SandboxClaim, *extensionsv1beta1.SandboxTemplate, *extensionsv1beta1.SandboxWarmPool, *sandboxv1beta1.Sandbox) {
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}
	warmSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       "warm-sb-uid",
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash("test-pool"),
				sandboxTemplateRefHash: SandboxTemplateRefHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxWarmPool",
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}
	return claim, template, warmPool, warmSandbox
}

// TestDisableFlagsWarmAdoptionClaimWrites verifies the write profile of a
// full warm-pool adoption reconcile with --disable-claim-events (nil
// recorder) and --disable-claim-observability-annotations both on: ZERO
// observability metadata patches land on the claim (the observability
// annotation write is gone), the only remaining claim writes are the
// adoption's optimistic-lock Update and the one-shot creation-latency
// -recorded stamp (#1114, a metrics-dedup correctness write outside this
// flag's scope), and the adoption still completes with the claim status
// bound to the warm sandbox. A control run with the flags off shows the one
// observability annotation patch the flag removes.
func TestDisableFlagsWarmAdoptionClaimWrites(t *testing.T) {
	run := func(t *testing.T, disableFlags bool) (obsPatches, latencyStampPatches, claimUpdates int, bound *extensionsv1beta1.SandboxClaim) {
		scheme := newScheme(t)
		claim, template, warmPool, warmSandbox := warmAdoptionFixtures()

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(template, warmPool, claim, warmSandbox).
			WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
						data, err := patch.Data(obj)
						if err != nil {
							t.Fatalf("compute patch data: %v", err)
						}
						// The creation-latency-recorded stamp (#1114) is a
						// separate metrics-dedup write, not governed by
						// --disable-claim-observability-annotations; bucket
						// it apart so the flag assertions stay precise.
						if strings.Contains(string(data), asmetrics.CreationLatencyRecordedAnnotation) {
							latencyStampPatches++
						} else {
							obsPatches++
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
						claimUpdates++
					}
					return c.Update(ctx, obj, opts...)
				},
			}).
			Build()

		warmSandboxQueue := queue.NewSimpleSandboxQueue()
		warmSandboxQueue.Add(
			queue.GetNamespacedWarmPoolName("default", "test-pool"),
			queue.SandboxKey{Namespace: "default", Name: "warm-sb"},
		)

		reconciler := &SandboxClaimReconciler{
			Client:                          fakeClient,
			Scheme:                          scheme,
			Tracer:                          asmetrics.NewNoOp(),
			WarmSandboxQueue:                warmSandboxQueue,
			DisableObservabilityAnnotations: disableFlags,
		}
		if disableFlags {
			// --disable-claim-events: nil recorder; every Eventf site is
			// nil-guarded so emission becomes a no-op.
			reconciler.Recorder = nil
		} else {
			reconciler.Recorder = events.NewFakeRecorder(10)
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		updatedClaim := &extensionsv1beta1.SandboxClaim{}
		if err := fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		return obsPatches, latencyStampPatches, claimUpdates, updatedClaim
	}

	t.Run("flags on: zero observability claim patches", func(t *testing.T) {
		obsPatches, latencyStampPatches, updates, claim := run(t, true)
		if obsPatches != 0 {
			t.Errorf("expected 0 observability claim patches with the flags enabled, got %d", obsPatches)
		}
		// The creation-latency-recorded stamp is a metrics-dedup correctness
		// write (#1114) and is expected regardless of the flags.
		if latencyStampPatches != 1 {
			t.Errorf("expected exactly 1 creation-latency-recorded stamp patch, got %d", latencyStampPatches)
		}
		// The adoption transaction's optimistic-lock Update (recording the
		// assigned sandbox on the claim) is a correctness write and stays.
		if updates != 1 {
			t.Errorf("expected exactly 1 claim update (adoption optimistic lock), got %d", updates)
		}
		// The adoption itself must still have completed: status bound to warm-sb.
		if claim.Status.SandboxStatus.Name != "warm-sb" {
			t.Errorf("claim status not bound to warm sandbox: %+v", claim.Status.SandboxStatus)
		}
	})

	t.Run("flags off control: one annotation patch", func(t *testing.T) {
		obsPatches, latencyStampPatches, updates, claim := run(t, false)
		if obsPatches != 1 {
			t.Errorf("expected exactly 1 claim metadata patch (observability annotations) with flags off, got %d", obsPatches)
		}
		if latencyStampPatches != 1 {
			t.Errorf("expected exactly 1 creation-latency-recorded stamp patch, got %d", latencyStampPatches)
		}
		if updates != 1 {
			t.Errorf("expected exactly 1 claim update (adoption optimistic lock), got %d", updates)
		}
		if claim.Status.SandboxStatus.Name != "warm-sb" {
			t.Errorf("claim status not bound to warm sandbox: %+v", claim.Status.SandboxStatus)
		}
		if claim.Annotations[asmetrics.ObservabilityAnnotation] == "" {
			t.Error("observability annotation should be persisted with flags off")
		}
	})
}
