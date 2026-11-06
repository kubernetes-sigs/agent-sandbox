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

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func nodeSpreadSandbox(name, node string) *sandboxv1beta1.Sandbox {
	sandbox := newPoolCandidateSandbox(name)
	sandbox.Status.NodeName = node
	return sandbox
}

func nodeSpreadKey(sandbox *sandboxv1beta1.Sandbox) queue.SandboxKey {
	return queue.SandboxKey{
		Namespace: sandbox.Namespace,
		Name:      sandbox.Name,
		NodeName:  sandbox.Status.NodeName,
	}
}

func requireNodeSpreadOrder(t *testing.T, q queue.SandboxQueue, pool string, sandboxes ...*sandboxv1beta1.Sandbox) {
	t.Helper()
	for _, sandbox := range sandboxes {
		key, ok := queue.GetWithNodeSpread(q, pool)
		require.True(t, ok, "expected candidate %s", sandbox.Name)
		require.Equal(t, nodeSpreadKey(sandbox), key)
	}
	_, ok := queue.GetWithNodeSpread(q, pool)
	require.False(t, ok, "all expected candidates have been removed")
}

func TestNodeSpreadSandboxEvents(t *testing.T) {
	tests := []struct {
		name     string
		changed  int
		node     string
		deleted  bool
		expected []int
	}{
		{name: "node migration changes largest bucket", changed: 2, node: "node-b", expected: []int{1, 0, 2, 3}},
		{name: "older migration preserves destination FIFO", changed: 0, node: "node-b", expected: []int{0, 1, 2, 3}},
		{name: "unscheduling preserves unscheduled FIFO", changed: 2, node: "", expected: []int{0, 1, 2, 3}},
		{name: "scheduling promotes pending candidate", changed: 3, node: "node-b", expected: []int{0, 1, 2, 3}},
		{name: "deletion with stale placement updates bucket counts", changed: 0, node: "stale-node", deleted: true, expected: []int{1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			q := queue.NewSimpleSandboxQueue()
			handler := &sandboxEventHandler{sandboxQueue: q}
			sandboxes := []*sandboxv1beta1.Sandbox{
				nodeSpreadSandbox("a-old", "node-a"),
				nodeSpreadSandbox("b-old", "node-b"),
				nodeSpreadSandbox("a-new", "node-a"),
				nodeSpreadSandbox("pending", ""),
			}
			for _, sandbox := range sandboxes {
				require.NoError(t, isAdoptable(sandbox))
				handler.Create(ctx, event.CreateEvent{Object: sandbox}, nil)
			}

			old := sandboxes[tt.changed]
			updated := old.DeepCopy()
			updated.Status.NodeName = tt.node
			if tt.deleted {
				handler.Delete(ctx, event.DeleteEvent{Object: updated}, nil)
			} else {
				handler.Update(ctx, event.UpdateEvent{ObjectOld: old, ObjectNew: updated}, nil)
				sandboxes[tt.changed] = updated
			}

			var expected []*sandboxv1beta1.Sandbox
			for _, i := range tt.expected {
				expected = append(expected, sandboxes[i])
			}
			requireNodeSpreadOrder(t, q, queue.GetNamespacedWarmPoolName("default", "test-pool"), expected...)
		})
	}
}

func TestNodeSpreadWarmPoolDeletion(t *testing.T) {
	ctx := context.Background()
	q := queue.NewSimpleSandboxQueue()
	sandboxHandler := &sandboxEventHandler{sandboxQueue: q}
	poolHandler := &warmPoolEventHandler{sandboxQueue: q}
	_, _, pool, _ := newOptimisticLockTestObjects()
	poolKey := queue.GetNamespacedWarmPoolName(pool.Namespace, pool.Name)
	for _, sandbox := range []*sandboxv1beta1.Sandbox{
		nodeSpreadSandbox("a-1", "node-a"),
		nodeSpreadSandbox("a-2", "node-a"),
		nodeSpreadSandbox("pending", ""),
	} {
		sandboxHandler.Create(ctx, event.CreateEvent{Object: sandbox}, nil)
	}
	other := nodeSpreadSandbox("other-namespace", "node-b")
	other.Namespace = "other"
	sandboxHandler.Create(ctx, event.CreateEvent{Object: other}, nil)

	poolHandler.Delete(ctx, event.DeleteEvent{Object: pool}, nil)
	requireNodeSpreadOrder(t, q, poolKey)
	_, ok := q.Get(poolKey)
	require.False(t, ok)
	requireNodeSpreadOrder(t, q, queue.GetNamespacedWarmPoolName(other.Namespace, pool.Name), other)

	replacement := nodeSpreadSandbox("replacement", "node-c")
	pending := nodeSpreadSandbox("new-pending", "")
	sandboxHandler.Create(ctx, event.CreateEvent{Object: pending}, nil)
	sandboxHandler.Create(ctx, event.CreateEvent{Object: replacement}, nil)
	requireNodeSpreadOrder(t, q, poolKey, replacement, pending)
}

func newNodeSpreadReconciler(t *testing.T, sandboxes ...*sandboxv1beta1.Sandbox) (*SandboxClaimReconciler, *extensionsv1beta1.SandboxClaim) {
	t.Helper()
	scheme := newScheme(t)
	claim, template, pool, _ := newOptimisticLockTestObjects()
	claim.Annotations = nil
	objects := []client.Object{claim, template, pool}
	q := queue.NewSimpleSandboxQueue()
	handler := &sandboxEventHandler{sandboxQueue: q}
	for _, sandbox := range sandboxes {
		require.NoError(t, verifySandboxCandidate(sandbox, claim))
		objects = append(objects, sandbox)
		handler.Create(context.Background(), event.CreateEvent{Object: sandbox}, nil)
	}
	return &SandboxClaimReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objects...).WithStatusSubresource(&sandboxv1beta1.Sandbox{}).Build(),
		Scheme:           scheme,
		WarmSandboxQueue: q,
		Tracer:           asmetrics.NewNoOp(),
	}, claim
}

type indexedNodeSpreadOnlyQueue struct {
	*queue.SimpleSandboxQueue
	t            *testing.T
	indexedCalls int
}

func (q *indexedNodeSpreadOnlyQueue) GetWithStrategy(string, func([]queue.SandboxKey) (queue.SandboxKey, bool)) (queue.SandboxKey, bool) {
	q.t.Fatal("getCandidate used the generic snapshot strategy")
	return queue.SandboxKey{}, false
}

func (q *indexedNodeSpreadOnlyQueue) GetWithNodeSpread(pool string) (queue.SandboxKey, bool) {
	q.indexedCalls++
	return q.SimpleSandboxQueue.GetWithNodeSpread(pool)
}

func TestNodeSpreadGetCandidateUsesIndex(t *testing.T) {
	candidate := nodeSpreadSandbox("ready", "node-a")
	reconciler, claim := newNodeSpreadReconciler(t, candidate)
	indexed := &indexedNodeSpreadOnlyQueue{
		SimpleSandboxQueue: reconciler.WarmSandboxQueue.(*queue.SimpleSandboxQueue),
		t:                  t,
	}
	reconciler.WarmSandboxQueue = indexed

	got, key, pending, err := reconciler.getCandidate(context.Background(), claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, candidate.Name, got.Name)
	require.Equal(t, nodeSpreadKey(candidate), key)
	require.Zero(t, pending)
	require.Equal(t, 1, indexed.indexedCalls)
}

func TestNodeSpreadGetCandidateReinsertion(t *testing.T) {
	ctx := context.Background()
	pending := nodeSpreadSandbox("pending", "node-a")
	pending.Status.PodIPs = nil
	fallback := nodeSpreadSandbox("fallback", "node-b")
	fallback.Status.Conditions[0].Status = metav1.ConditionFalse
	secondFallback := nodeSpreadSandbox("second-fallback", "node-c")
	secondFallback.Status.Conditions[0].Status = metav1.ConditionFalse
	ready := nodeSpreadSandbox("ready", "node-d")
	reconciler, claim := newNodeSpreadReconciler(t, pending, fallback, secondFallback, ready)
	poolKey := queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name)

	// The cached Sandbox can learn its placement before its watch event arrives.
	updated := pending.DeepCopy()
	updated.Status.NodeName = "node-d"
	require.NoError(t, reconciler.Status().Update(ctx, updated))

	got, key, count, err := reconciler.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, ready.Name, got.Name)
	require.Equal(t, nodeSpreadKey(ready), key)
	require.Equal(t, 1, count)

	// A later claim can use the reinserted candidates once they become Ready.
	for _, sandbox := range []*sandboxv1beta1.Sandbox{updated, secondFallback, fallback} {
		sandbox.Status.PodIPs = []string{testNetworkedPodIP}
		sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
		require.NoError(t, reconciler.Status().Update(ctx, sandbox))
	}
	for _, want := range []*sandboxv1beta1.Sandbox{updated, secondFallback, fallback} {
		got, key, count, err = reconciler.getCandidate(ctx, claim)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, want.Name, got.Name)
		require.Equal(t, nodeSpreadKey(want), key)
		require.Zero(t, count)
	}
	requireNodeSpreadOrder(t, reconciler.WarmSandboxQueue, poolKey)
}

func TestNodeSpreadGetCandidateFallback(t *testing.T) {
	first := nodeSpreadSandbox("first-unready", "node-a")
	first.Status.Conditions[0].Status = metav1.ConditionFalse
	second := nodeSpreadSandbox("second-unready", "node-b")
	second.Status.Conditions[0].Status = metav1.ConditionFalse
	pending := nodeSpreadSandbox("pending", "")
	pending.Status.PodIPs = nil
	reconciler, claim := newNodeSpreadReconciler(t, first, second, pending)

	got, key, count, err := reconciler.getCandidate(context.Background(), claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, first.Name, got.Name)
	require.Equal(t, nodeSpreadKey(first), key)
	require.Equal(t, 1, count)
	requireNodeSpreadOrder(t, reconciler.WarmSandboxQueue,
		queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name), second, pending)
}

func TestNodeSpreadGetCandidateReadErrorReinsertion(t *testing.T) {
	ctx := context.Background()
	pending := nodeSpreadSandbox("pending", "node-a")
	pending.Status.PodIPs = nil
	fallback := nodeSpreadSandbox("fallback", "node-b")
	fallback.Status.Conditions[0].Status = metav1.ConditionFalse
	failed := nodeSpreadSandbox("read-error", "node-c")
	reconciler, claim := newNodeSpreadReconciler(t, pending, fallback, failed)
	readErr := errors.New("temporary sandbox read failure")
	failRead := true
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == failed.Name && failRead {
				return readErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	got, key, count, err := reconciler.getCandidate(ctx, claim)
	require.ErrorIs(t, err, readErr)
	require.Nil(t, got)
	require.Equal(t, queue.SandboxKey{}, key)
	require.Equal(t, 1, count)

	failRead = false
	got, key, count, err = reconciler.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, failed.Name, got.Name)
	require.Equal(t, nodeSpreadKey(failed), key)
	require.Zero(t, count)
	requireNodeSpreadOrder(t, reconciler.WarmSandboxQueue,
		queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name), pending, fallback)
}

func TestNodeSpreadGetCandidatePoolErrorReinsertion(t *testing.T) {
	ctx := context.Background()
	pending := nodeSpreadSandbox("pending", "node-a")
	pending.Status.PodIPs = nil
	ready := nodeSpreadSandbox("ready", "node-b")
	reconciler, claim := newNodeSpreadReconciler(t, pending, ready)
	failRead := true
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*extensionsv1beta1.SandboxWarmPool); ok && failRead {
				return errors.New("temporary warm pool read failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	got, key, count, err := reconciler.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Equal(t, queue.SandboxKey{}, key)
	require.Equal(t, 1, count)

	failRead = false
	got, key, count, err = reconciler.getCandidate(ctx, claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, ready.Name, got.Name)
	require.Equal(t, nodeSpreadKey(ready), key)
	require.Equal(t, 1, count)
	requireNodeSpreadOrder(t, reconciler.WarmSandboxQueue,
		queue.GetNamespacedWarmPoolName(claim.Namespace, claim.Spec.WarmPoolRef.Name), pending)
}
