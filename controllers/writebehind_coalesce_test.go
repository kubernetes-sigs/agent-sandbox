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

// RequeueAfter write-deferral tests: opt-in deferral of the
// sandbox controller's recoverable metadata-only writes
// (--sandbox-write-behind-window), including the flag-off (window=0)
// synchronous identity, workqueue-driven per-object coalescing, the
// readiness-never-gated guarantee, and level-based crash recovery.

import (
	"context"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const (
	wbNamespace = "wb-ns"
	wbSandbox   = "wb-sandbox"
	// autoscalerSafeToEvictAnnotation marks a pod as evictable by the cluster
	// autoscaler. The warm pool stamps it "true" on pool-owned pods so idle
	// warm capacity can be scaled down; once a pod backs a claimed sandbox it
	// must NOT carry the marker (an eviction would kill an in-use sandbox).
	autoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

func claimOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       "wb-claim",
		UID:        "wb-claim-uid",
		Controller: new(true),
	}
}

// postAdoptionFixture models the state the sandbox controller observes right
// after a warm-pool adoption: the sandbox is claim-owned and its template no
// longer carries the safe-to-evict marker (deleted by the claim controller's
// spec rewrite), while the live pod still does (plus the warm-pool label).
// The pod metadata reconciliation must strip both and update the tracking
// annotations.
func postAdoptionFixture() (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	hash := NameHash(wbSandbox)
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       wbSandbox,
			Namespace:  wbNamespace,
			UID:        sandboxUID,
			Generation: 3,
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: wbSandbox,
			},
			OwnerReferences: []metav1.OwnerReference{claimOwnerRef()},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
					// Post-adoption template: safe-to-evict deleted by the
					// claim controller's spec rewrite.
					ObjectMeta: sandboxv1beta1.PodMetadata{},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wbSandbox,
			Namespace: wbNamespace,
			Labels: map[string]string{
				sandboxLabel:                        hash,
				sandboxv1beta1.SandboxWarmPoolLabel: NameHash("wb-pool"),
			},
			Annotations: map[string]string{
				autoscalerSafeToEvictAnnotation:                       "true",
				sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation: autoscalerSafeToEvictAnnotation,
			},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(wbSandbox)},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "c"}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIPs:     []corev1.PodIP{{IP: "10.0.0.9"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	return sb, pod
}

type wbCounters struct {
	podPatches         int
	sandboxPatches     int
	subResourcePatches int
}

func (w *wbCounters) interceptors() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			switch obj.(type) {
			case *corev1.Pod:
				w.podPatches++
			case *sandboxv1beta1.Sandbox:
				w.sandboxPatches++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			w.subResourcePatches++
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			w.subResourcePatches++
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}
}

func newWbClient(counters *wbCounters, objs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithInterceptorFuncs(counters.interceptors()).
		WithRuntimeObjects(objs...).
		Build()
}

// newWbReconciler builds a SandboxReconciler; window == 0 models the default
// flag-off configuration (fully synchronous writes), exactly as
// cmd/agent-sandbox-controller wires it. window > 0 enables the
// RequeueAfter-based deferral of the recoverable pod metadata patch.
func newWbReconciler(t *testing.T, cl client.Client, window time.Duration) *SandboxReconciler {
	t.Helper()
	return &SandboxReconciler{
		Client:            cl,
		Scheme:            Scheme,
		Tracer:            asmetrics.NewNoOp(),
		ClusterDomain:     "cluster.local",
		WriteBehindWindow: window,
	}
}

func wbReconcile(t *testing.T, r *SandboxReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wbSandbox, Namespace: wbNamespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getWbPod(t *testing.T, cl client.Client) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: wbSandbox, Namespace: wbNamespace}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return pod
}

func getWbSandbox(t *testing.T, cl client.Client) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: wbSandbox, Namespace: wbNamespace}, sb); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return sb
}

func wbReadyCondition(sb *sandboxv1beta1.Sandbox) *metav1.Condition {
	for i := range sb.Status.Conditions {
		if sb.Status.Conditions[i].Type == string(sandboxv1beta1.SandboxConditionReady) {
			return &sb.Status.Conditions[i]
		}
	}
	return nil
}

// TestWriteBehindDisabledIsSynchronous pins the flag-off identity: with
// window=0 the reconcile runs the stock synchronous path — the pod metadata
// patch is issued inline, exactly once, during the reconcile, and the pod
// converges before Reconcile returns.
func TestWriteBehindDisabledIsSynchronous(t *testing.T) {
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)
	r := newWbReconciler(t, cl, 0)

	wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("synchronous mode: %d pod patches during reconcile, want exactly 1", counters.podPatches)
	}
	got := getWbPod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker not stripped synchronously during reconcile")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label not stripped synchronously during reconcile")
	}

	// Idempotence: a second reconcile is pod-write-free.
	wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("second synchronous reconcile issued %d extra pod patches, want 0", counters.podPatches-1)
	}
}

// TestRequeueDeferralCoalescesAdoptionPodPatch: with the deferral enabled,
// the adoption-path pod metadata reconciliation issues ZERO pod patches
// while the window is open and instead returns RequeueAfter (bounded by the
// 1s pod-patch cap); repeated redeliveries within the window coalesce (still
// zero writes, the deferral clock is NOT re-armed); the pass that runs after
// the window elapses recomputes the drift from informer state and issues
// exactly ONE patch whose result is byte-identical to synchronous mode; and
// readiness is NEVER gated on the deferred write — the deferring pass
// finalizes the same status the synchronous pass would.
func TestRequeueDeferralCoalescesAdoptionPodPatch(t *testing.T) {
	// Reference run: synchronous mode (window=0, flag off).
	syncCounters := &wbCounters{}
	sbRef, podRef := postAdoptionFixture()
	syncClient := newWbClient(syncCounters, sbRef, podRef)
	syncR := newWbReconciler(t, syncClient, 0)
	wbReconcile(t, syncR)
	if syncCounters.podPatches != 1 {
		t.Fatalf("synchronous mode: %d pod patches during reconcile, want 1 (flag-off identity)", syncCounters.podPatches)
	}
	wantPod := getWbPod(t, syncClient)
	wantReady := wbReadyCondition(getWbSandbox(t, syncClient))

	// Deferral run. A large flag window exercises the pod-patch bound:
	// the effective window must be capped at podMetadataFlushBound (1s).
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)
	r := newWbReconciler(t, cl, time.Hour)

	res := wbReconcile(t, r)
	if counters.podPatches != 0 {
		t.Fatalf("deferral mode: %d pod patches during reconcile, want 0 (deferred)", counters.podPatches)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > podMetadataFlushBound {
		t.Fatalf("deferring pass returned RequeueAfter=%v, want in (0, %v] (pod-patch bound caps the flag window)", res.RequeueAfter, podMetadataFlushBound)
	}
	if got := getWbPod(t, cl); got.Annotations[autoscalerSafeToEvictAnnotation] != "true" {
		t.Fatal("pod mutated on the server before the deferral window elapsed")
	}

	// CRITICAL readiness guarantee: the deferring pass must have finalized
	// the sandbox status identically to the synchronous pass — the ready
	// path never waits on the deferred write.
	if counters.subResourcePatches == 0 {
		t.Fatal("deferring pass issued no status write: readiness was gated on the deferred patch")
	}
	gotReady := wbReadyCondition(getWbSandbox(t, cl))
	if (wantReady == nil) != (gotReady == nil) {
		t.Fatalf("Ready condition presence diverges from synchronous mode: sync=%v deferral=%v", wantReady, gotReady)
	}
	if wantReady != nil && gotReady.Status != wantReady.Status {
		t.Fatalf("Ready condition diverges from synchronous mode: sync=%s deferral=%s", wantReady.Status, gotReady.Status)
	}

	// A second redelivery inside the window recomputes the same drift and
	// coalesces: still zero patches, and the deferral clock is NOT re-armed
	// (RequeueAfter shrinks toward the original deadline, never grows).
	res2 := wbReconcile(t, r)
	if counters.podPatches != 0 {
		t.Fatalf("second reconcile issued %d pod patches, want 0 (coalesced)", counters.podPatches)
	}
	if res2.RequeueAfter <= 0 || res2.RequeueAfter > res.RequeueAfter {
		t.Fatalf("second deferring pass returned RequeueAfter=%v, want in (0, %v] (window must not re-arm)", res2.RequeueAfter, res.RequeueAfter)
	}

	// The requeued pass after the window elapses flushes exactly once.
	r.deferralClock.now = func() time.Time { return time.Now().Add(2 * podMetadataFlushBound) }
	res3 := wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("after the window: %d pod patches, want exactly 1 for all deferred detections", counters.podPatches)
	}

	got := getWbPod(t, cl)
	if !maps.Equal(got.Labels, wantPod.Labels) {
		t.Errorf("flushed labels diverge from synchronous mode:\n got %v\nwant %v", got.Labels, wantPod.Labels)
	}
	if !maps.Equal(got.Annotations, wantPod.Annotations) {
		t.Errorf("flushed annotations diverge from synchronous mode:\n got %v\nwant %v", got.Annotations, wantPod.Annotations)
	}
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker survived the flush")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label survived the flush")
	}

	// The flushing pass cleared its deferral clock entry and did not requeue
	// for deferral reasons.
	if n := len(r.deferralClock.first); n != 0 {
		t.Fatalf("deferral clock entries after flush = %d, want 0", n)
	}
	_ = res3

	// A converged follow-up pass performs no pod writes and books no deferral.
	wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("converged pass issued %d extra pod patches, want 0", counters.podPatches-1)
	}
	if n := len(r.deferralClock.first); n != 0 {
		t.Fatalf("converged pass left %d deferral clock entries, want 0", n)
	}
}

// TestRequeueDeferralCrashRecovery: the deferral clock lost with the process
// holds NO mutation payload — a fresh reconciler (new process) recomputes
// the pending write from informer state alone. The only cost of the crash is
// a restarted (sub-second) deferral window on the replacement.
func TestRequeueDeferralCrashRecovery(t *testing.T) {
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)

	// Process 1 defers but "crashes" before its requeue fires.
	r1 := newWbReconciler(t, cl, 250*time.Millisecond)
	wbReconcile(t, r1)
	if counters.podPatches != 0 {
		t.Fatalf("pre-crash: %d pod patches, want 0", counters.podPatches)
	}

	// Process 2 (fresh clock) re-detects the drift; the window restarts.
	r2 := newWbReconciler(t, cl, 250*time.Millisecond)
	res := wbReconcile(t, r2)
	if counters.podPatches != 0 {
		t.Fatalf("post-crash first pass: %d pod patches, want 0 (window restarts)", counters.podPatches)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 250*time.Millisecond {
		t.Fatalf("post-crash deferring pass returned RequeueAfter=%v, want in (0, 250ms]", res.RequeueAfter)
	}

	// Its requeued pass flushes the exact mutation.
	r2.deferralClock.now = func() time.Time { return time.Now().Add(time.Second) }
	wbReconcile(t, r2)
	if counters.podPatches != 1 {
		t.Fatalf("post-crash flush issued %d pod patches, want 1", counters.podPatches)
	}
	got := getWbPod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker not stripped by the recovery flush")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label not stripped by the recovery flush")
	}
}
