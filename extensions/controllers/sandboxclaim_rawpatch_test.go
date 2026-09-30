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
	"encoding/json"
	"maps"
	"slices"
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
	pending := r.initializeAnnotations(context.Background(), live)
	if len(pending) != 2 {
		t.Fatalf("expected both annotations pending persistence, got %v", pending)
	}
	if captured != nil {
		t.Fatalf("initializeAnnotations must only stamp in-memory; the write is deferred to the flush, got %s", captured)
	}
	if err := r.flushPendingAnnotations(context.Background(), live, pending); err != nil {
		t.Fatalf("flushPendingAnnotations failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a successful flush must clear the pending set, got %v", pending)
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

	// Idempotence short-circuit: annotations already present leave nothing
	// pending, so the flush makes no API call.
	captured = nil
	pending = r.initializeAnnotations(context.Background(), live)
	if pending != nil {
		t.Errorf("expected nothing pending when annotations already present, got %v", pending)
	}
	if err := r.flushPendingAnnotations(context.Background(), live, pending); err != nil {
		t.Fatalf("flush with nothing pending failed: %v", err)
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
	pending := r.initializeAnnotations(context.Background(), live)
	if pending != nil {
		t.Errorf("expected nothing pending persistence with the flag enabled, got %v", pending)
	}
	if err := r.flushPendingAnnotations(context.Background(), live, pending); err != nil {
		t.Fatalf("flushPendingAnnotations failed: %v", err)
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
			PodIPs: []string{testNetworkedPodIP},
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}
	return claim, template, warmPool, warmSandbox
}

// claimWriteProfile is the per-claim write ledger of one adoption reconcile,
// bucketed by what each write carries.
type claimWriteProfile struct {
	adoptionPatchBodies []string // main-resource patches carrying the assignment annotation
	obsPatches          int      // standalone observability annotation patches
	firstReadyPatches   int      // persistent first-ready stamp patches
	statusPatches       int
	updates             int // full-object claim Updates (must stay 0)
	bound               *extensionsv1beta1.SandboxClaim
}

func (p claimWriteProfile) total() int {
	return len(p.adoptionPatchBodies) + p.obsPatches + p.firstReadyPatches + p.statusPatches + p.updates
}

// TestDisableFlagsWarmAdoptionClaimWrites pins the write profile of a full
// warm-pool adoption reconcile, with and without --disable-claim-events (nil
// recorder) and --disable-claim-observability-annotations:
//
//   - flags off: ONE adoption patch that carries the assignment annotation
//     together with the observability annotations stamped earlier in the
//     pass (no standalone observability patch), the status patch, and the
//     first-ready stamp — three claim writes;
//   - flags on: the adoption patch (assignment only) and the status patch —
//     two claim writes; the first-ready flap guard moves in memory.
//
// In both runs the adoption write is a metadata-only, optimistically locked
// merge patch and never a full-object Update: the old Update re-serialized
// the user-owned spec (spec.additionalPodMetadata as {} when the stored
// object omitted it), which the API server counted as a spec change and
// answered with a metadata.generation bump on a claim the user never edited.
func TestDisableFlagsWarmAdoptionClaimWrites(t *testing.T) {
	run := func(t *testing.T, disableFlags bool) claimWriteProfile {
		scheme := newScheme(t)
		claim, template, warmPool, warmSandbox := warmAdoptionFixtures()
		claim.Generation = 1

		var profile claimWriteProfile
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
						body := string(data)
						switch {
						case strings.Contains(body, extensionsv1beta1.AssignedSandboxNameAnnotation):
							profile.adoptionPatchBodies = append(profile.adoptionPatchBodies, body)
						case strings.Contains(body, asmetrics.ClaimFirstReadyAnnotation):
							profile.firstReadyPatches++
						default:
							profile.obsPatches++
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
						profile.statusPatches++
					}
					return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
						profile.updates++
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

		profile.bound = &extensionsv1beta1.SandboxClaim{}
		if err := fakeClient.Get(context.Background(), req.NamespacedName, profile.bound); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		return profile
	}

	// assertAdoptionPatch checks the invariants shared by both runs: exactly
	// one adoption write, a metadata-only body ({"metadata":{"annotations":
	// ...,"resourceVersion":...}} and nothing else), the expected annotation
	// keys, no full-object Update, and an unchanged metadata.generation. The
	// fake client does not model the API server's generation bump, so the
	// body shape is the load-bearing assertion; the generation check pins the
	// contract the shape guarantees.
	assertAdoptionPatch := func(t *testing.T, profile claimWriteProfile, wantAnnotationKeys []string) {
		t.Helper()
		if profile.updates != 0 {
			t.Fatalf("claim must never be written with a full-object Update, got %d", profile.updates)
		}
		if len(profile.adoptionPatchBodies) != 1 {
			t.Fatalf("expected exactly 1 adoption patch, got %d: %v", len(profile.adoptionPatchBodies), profile.adoptionPatchBodies)
		}
		body := profile.adoptionPatchBodies[0]
		var top map[string]map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &top); err != nil {
			t.Fatalf("adoption patch is not a metadata object: %v (%s)", err, body)
		}
		if got := slices.Sorted(maps.Keys(top)); !slices.Equal(got, []string{"metadata"}) {
			t.Errorf("adoption patch must touch only metadata, got top-level keys %v: %s", got, body)
		}
		if got := slices.Sorted(maps.Keys(top["metadata"])); !slices.Equal(got, []string{"annotations", "resourceVersion"}) {
			t.Errorf("adoption patch metadata must be annotations + optimistic-lock resourceVersion, got %v: %s", got, body)
		}
		var annotations map[string]string
		if err := json.Unmarshal(top["metadata"]["annotations"], &annotations); err != nil {
			t.Fatalf("decode adoption patch annotations: %v (%s)", err, body)
		}
		if got := slices.Sorted(maps.Keys(annotations)); !slices.Equal(got, wantAnnotationKeys) {
			t.Errorf("adoption patch annotation keys = %v, want %v: %s", got, wantAnnotationKeys, body)
		}
		if annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] != "warm-sb" {
			t.Errorf("adoption patch must record warm-sb, got %q", annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		}
		if profile.bound.Generation != 1 {
			t.Errorf("adoption must not change metadata.generation, got %d", profile.bound.Generation)
		}
		if profile.bound.Status.SandboxStatus.Name != "warm-sb" {
			t.Errorf("claim status not bound to warm sandbox: %+v", profile.bound.Status.SandboxStatus)
		}
		if profile.statusPatches != 1 {
			t.Errorf("expected exactly 1 status patch, got %d", profile.statusPatches)
		}
	}

	t.Run("flags off: observability annotations fold into the adoption patch", func(t *testing.T) {
		profile := run(t, false)
		// Sorted key order, as the body is emitted.
		assertAdoptionPatch(t, profile, []string{asmetrics.ObservabilityAnnotation, extensionsv1beta1.AssignedSandboxNameAnnotation})
		if profile.obsPatches != 0 {
			t.Errorf("expected no standalone observability patch (it rides on the adoption patch), got %d", profile.obsPatches)
		}
		if profile.firstReadyPatches != 1 {
			t.Errorf("expected exactly 1 first-ready stamp patch, got %d", profile.firstReadyPatches)
		}
		if profile.total() != 3 {
			t.Errorf("expected 3 claim writes per adoption with defaults, got %d: %+v", profile.total(), profile)
		}
		if profile.bound.Annotations[asmetrics.ObservabilityAnnotation] == "" {
			t.Error("observability annotation should be persisted with flags off")
		}
		if profile.bound.Annotations[asmetrics.ClaimFirstReadyAnnotation] == "" {
			t.Error("first-ready annotation should be persisted with flags off")
		}
	})

	t.Run("flags on: adoption and status patches only", func(t *testing.T) {
		profile := run(t, true)
		assertAdoptionPatch(t, profile, []string{extensionsv1beta1.AssignedSandboxNameAnnotation})
		if profile.obsPatches != 0 {
			t.Errorf("expected 0 observability claim patches with the flags enabled, got %d", profile.obsPatches)
		}
		if profile.firstReadyPatches != 0 {
			t.Errorf("expected the first-ready stamp write to be suppressed by the flag, got %d", profile.firstReadyPatches)
		}
		if profile.total() != 2 {
			t.Errorf("expected 2 claim writes per adoption with the flags on, got %d: %+v", profile.total(), profile)
		}
		for _, key := range []string{asmetrics.ObservabilityAnnotation, asmetrics.TraceContextAnnotation, asmetrics.ClaimFirstReadyAnnotation} {
			if v := profile.bound.Annotations[key]; v != "" {
				t.Errorf("observability annotation %s must not be persisted with the flag on, got %q", key, v)
			}
		}
	})
}
