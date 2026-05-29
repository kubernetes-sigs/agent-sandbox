package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// These tests document why the Sandbox controller writes its status with a
// non-optimistic merge Patch instead of Update.
//
// Under burst load (for example a warm pool draining and re-minting many
// sandboxes at once), the controller issues many status writes in quick
// succession. With Status().Update(), each write carries a ResourceVersion
// precondition; when the controller's cache lags its own writes the cached
// ResourceVersion is stale and the apiserver rejects the write with a 409
// Conflict. The controller then requeues and retries, producing a
// conflict/retry storm that starves forward progress. Switching to a merge
// Patch (which sends no ResourceVersion precondition) makes status writes
// resilient to a stale cache.

func newSandboxForStatusTest() *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "status-conflict-sandbox",
			Namespace: "default",
		},
		Spec: sandboxv1beta1.SandboxSpec{
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
		},
	}
}

func newStatusTestReconciler(objs ...runtime.Object) SandboxReconciler {
	return SandboxReconciler{
		Client:        newFakeClient(objs...),
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}
}

// TestStatusUpdateConflictsOnStaleResourceVersion documents the bug: a
// Status().Update() with a stale ResourceVersion is rejected with a Conflict.
func TestStatusUpdateConflictsOnStaleResourceVersion(t *testing.T) {
	ctx := t.Context()
	sb := newSandboxForStatusTest()
	r := newStatusTestReconciler(sb)
	key := types.NamespacedName{Name: sb.Name, Namespace: sb.Namespace}

	// A copy held as the controller's cache would hold it.
	stale := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, stale))

	// A concurrent writer advances the server's ResourceVersion.
	fresh := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, fresh))
	fresh.Status.Replicas = 1
	require.NoError(t, r.Status().Update(ctx, fresh))

	// The stale writer's Update is now rejected with a 409 Conflict.
	stale.Status.Replicas = 2
	err := r.Status().Update(ctx, stale)
	require.Error(t, err)
	require.Truef(t, k8serrors.IsConflict(err), "expected a Conflict error, got %v", err)
}

// TestUpdateStatusSucceedsWithStaleResourceVersion proves the fix: updateStatus
// uses a merge Patch, so the same stale ResourceVersion does not cause a conflict.
func TestUpdateStatusSucceedsWithStaleResourceVersion(t *testing.T) {
	ctx := t.Context()
	sb := newSandboxForStatusTest()
	r := newStatusTestReconciler(sb)
	key := types.NamespacedName{Name: sb.Name, Namespace: sb.Namespace}

	stale := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, stale))

	// Advance the server ResourceVersion out from under the stale copy.
	fresh := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, fresh))
	fresh.Status.Replicas = 1
	require.NoError(t, r.Status().Update(ctx, fresh))

	// updateStatus must succeed despite the stale ResourceVersion.
	oldStatus := stale.Status.DeepCopy()
	stale.Status.Replicas = 2
	require.NoError(t, r.updateStatus(ctx, oldStatus, stale))

	got := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, got))
	require.Equal(t, int32(2), got.Status.Replicas)
}

// TestUpdateStatusIgnoresDeletedSandbox proves the second half of the fix:
// writing status to a Sandbox that was deleted mid-reconcile must not error
// (which would cause a pointless requeue).
func TestUpdateStatusIgnoresDeletedSandbox(t *testing.T) {
	ctx := t.Context()
	sb := newSandboxForStatusTest()
	r := newStatusTestReconciler(sb)
	key := types.NamespacedName{Name: sb.Name, Namespace: sb.Namespace}

	current := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(ctx, key, current))

	// The sandbox is deleted out from under the reconcile.
	require.NoError(t, r.Delete(ctx, current))

	// Writing status to the now-deleted object must return nil, not an error.
	oldStatus := current.Status.DeepCopy()
	current.Status.Replicas = 2
	require.NoError(t, r.updateStatus(ctx, oldStatus, current))
}

func TestIsPodReady(t *testing.T) {
	notReady := &corev1.Pod{}
	require.False(t, isPodReady(notReady))

	ready := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}}
	require.True(t, isPodReady(ready))
}

func TestPodStatusChanged(t *testing.T) {
	base := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodPending,
		PodIPs:     []corev1.PodIP{{IP: "10.0.0.1"}},
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
	}}

	// No relevant change.
	require.False(t, podStatusChanged(base, base.DeepCopy()))

	// Phase change.
	phaseChanged := base.DeepCopy()
	phaseChanged.Status.Phase = corev1.PodRunning
	require.True(t, podStatusChanged(base, phaseChanged))

	// Ready flip.
	readyFlipped := base.DeepCopy()
	readyFlipped.Status.Conditions[0].Status = corev1.ConditionTrue
	require.True(t, podStatusChanged(base, readyFlipped))

	// PodIPs change without phase/ready change (the reviewer's concern).
	ipsChanged := base.DeepCopy()
	ipsChanged.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}}
	require.True(t, podStatusChanged(base, ipsChanged))
}
