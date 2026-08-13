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

// Regression tests for #1215: SandboxWarmPool over-creates replicas.
//
// The production failure mode: reconcilePool computes
// "toCreate = spec.replicas - len(cachedSandboxes)" from the informer cache,
// which lags the controller's own just-issued creates. Every create event
// re-enqueues the pool (ownership watch), so under load the same pool is
// re-reconciled while the cache still shows the pre-create state, and each
// pass creates toward the target again — ~10x over-creation observed at
// --sandbox-warm-pool-concurrent-workers=1000 (>=5,000 creates for a 500-pod
// target; log-confirmed by tomergee on the issue).
package controllers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// laggingClient wraps the fake client to model an informer cache that has NOT
// yet observed the controller's own writes: sandboxes created through it stay
// invisible to List until catchUp() is called. This reproduces the stale-read
// window in which the #1215 over-creation happens.
type laggingClient struct {
	client.WithWatch

	mu      sync.Mutex
	hidden  map[string]struct{} // namespace/name of creates not yet "cache-visible"
	creates int
}

func newLaggingClient(inner client.WithWatch) *laggingClient {
	return &laggingClient{WithWatch: inner, hidden: map[string]struct{}{}}
}

func (c *laggingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.WithWatch.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*sandboxv1beta1.Sandbox); !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	c.hidden[obj.GetNamespace()+"/"+obj.GetName()] = struct{}{}
	return nil
}

func (c *laggingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.WithWatch.List(ctx, list, opts...); err != nil {
		return err
	}
	sl, ok := list.(*sandboxv1beta1.SandboxList)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hidden) == 0 {
		return nil
	}
	filtered := sl.Items[:0]
	for _, item := range sl.Items {
		if _, isHidden := c.hidden[item.Namespace+"/"+item.Name]; !isHidden {
			filtered = append(filtered, item)
		}
	}
	sl.Items = filtered
	return nil
}

// catchUp makes all previous creates cache-visible (the informer caught up).
func (c *laggingClient) catchUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hidden = map[string]struct{}{}
}

func (c *laggingClient) createCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates
}

func newOverCreationFixture(replicas int32) (*extensionsv1beta1.SandboxWarmPool, *extensionsv1beta1.SandboxTemplate) {
	template := createTemplate("default")
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
			UID:       "warmpool-uid-1215",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	return warmPool, template
}

// TestReconcilePool_NoOverCreationWithStaleCache is the #1215 repro shape:
// repeated reconciles of the same pool against a List that lags behind the
// controller's own creates.
//
// Repro proof: this test catches the original bug. Verified during
// development by disabling the expectations gate (forcing TryExpectCreations
// to return true): every pass then re-created toward the target off the
// stale list and the test failed with 15 creates for replicas=3 after 5
// reconciles — the exact over-creation mechanism from the issue. With the
// gate in place, total creates == spec.replicas exactly.
func TestReconcilePool_NoOverCreationWithStaleCache(t *testing.T) {
	const replicas = int32(3)
	warmPool, template := newOverCreationFixture(replicas)
	scheme := newTestScheme()
	lc := newLaggingClient(newFakeClient(scheme, template, warmPool))

	r := SandboxWarmPoolReconciler{
		Client:       lc,
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
	}
	ctx := context.Background()

	// Reconcile the pool 5 times while the "cache" still reports the
	// pre-create state (models the rapid watch-triggered re-reconciles that
	// amplified the bug at high worker concurrency).
	for range 5 {
		_, err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
	}
	require.Equal(t, int(replicas), lc.createCount(),
		"total creates must equal spec.replicas even when List lags behind Creates")

	// The cache catches up and the watch observes the adds (production path:
	// warmPoolSandboxEventHandler.Create). Exercise the real handler so the
	// observation path is covered end to end.
	lc.catchUp()
	created := &sandboxv1beta1.SandboxList{}
	require.NoError(t, lc.List(ctx, created, client.InNamespace(warmPool.Namespace)))
	require.Len(t, created.Items, int(replicas))
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
	for i := range created.Items {
		h.Create(ctx, event.CreateEvent{Object: &created.Items[i]}, nil)
	}
	poolKey := types.NamespacedName{Namespace: warmPool.Namespace, Name: warmPool.Name}
	require.True(t, r.exp().SatisfiedExpectations(poolKey),
		"watch-observed adds must satisfy the recorded expectations")

	// Further reconciles are steady-state: no additional creates.
	for range 3 {
		_, err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
	}
	require.Equal(t, int(replicas), lc.createCount(), "steady state must not create more sandboxes")
	require.Equal(t, replicas, warmPool.Status.Replicas)
}

// TestReconcilePool_NoOverCreationConcurrent asserts the create gate is
// atomic under truly parallel reconciles of the same pool (run with -race).
func TestReconcilePool_NoOverCreationConcurrent(t *testing.T) {
	const replicas = int32(3)
	warmPool, template := newOverCreationFixture(replicas)
	scheme := newTestScheme()
	lc := newLaggingClient(newFakeClient(scheme, template, warmPool))

	r := SandboxWarmPoolReconciler{
		Client:       lc,
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
	}
	ctx := context.Background()

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			// Each reconcile works on its own copy, as real reconciles do
			// (each Reconcile call Gets a fresh object).
			_, errs[n] = r.reconcilePool(ctx, warmPool.DeepCopy())
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int(replicas), lc.createCount(),
		"concurrent reconciles of the same pool must not over-create")
}

// TestReconcilePool_TerminatingCountsAgainstTarget: a pool-owned sandbox that
// is terminating (deletionTimestamp set, still present) no longer counts as
// active, but it still occupies capacity — the create path must count it
// against spec.replicas instead of racing a replacement into the cluster
// while the delete drains (#1215 delete-lag ballooning).
func TestReconcilePool_TerminatingCountsAgainstTarget(t *testing.T) {
	const poolName = "test-pool"
	const poolNamespace = "default"
	replicas := int32(2)
	template := createTemplate(poolNamespace)
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-1215",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}

	ownedBy := func(sb *sandboxv1beta1.Sandbox, uid types.UID) *sandboxv1beta1.Sandbox {
		controller := true
		sb.UID = uid
		sb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
			Name:       poolName,
			UID:        warmPool.UID,
			Controller: &controller,
		}}
		return sb
	}

	active := ownedBy(createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-active"), "uid-active")
	terminating := ownedBy(createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-terminating"), "uid-terminating")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	// A finalizer keeps the fake object present-while-terminating, like a
	// real sandbox draining behind its finalizer.
	terminating.Finalizers = []string{"test.agents.x-k8s.io/drain"}

	scheme := newTestScheme()
	lc := newLaggingClient(newFakeClient(scheme, template, warmPool, active, terminating))
	r := SandboxWarmPoolReconciler{
		Client:       lc,
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
	}
	ctx := context.Background()

	_, err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	// Population = 1 active + 1 terminating = spec.replicas: no create.
	// (The old code excluded terminating sandboxes entirely and would have
	// created a replacement here, ballooning the population to 3.)
	require.Equal(t, 0, lc.createCount(),
		"terminating sandbox must count against the create target while still present")
	// Terminating sandboxes are excluded from status accounting.
	require.Equal(t, int32(1), warmPool.Status.Replicas)

	// Once the terminating sandbox is fully gone, the replacement is created.
	terminating.Finalizers = nil
	require.NoError(t, lc.Update(ctx, terminating))
	require.NoError(t, client.IgnoreNotFound(lc.Delete(ctx, terminating)))

	_, err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	require.Equal(t, 1, lc.createCount(), "replacement is created once the terminating sandbox is gone")
}

// TestReconcilePool_UnschedulableStuckGC: a non-Ready sandbox past the
// readiness grace period whose backing pod is unschedulable must be HELD, not
// deleted — deleting it would just create an equally unschedulable
// replacement in an unbounded loop (#1215). A genuinely stuck sandbox (pod
// scheduled or missing) is still replaced as before.
func TestReconcilePool_UnschedulableStuckGC(t *testing.T) {
	const poolName = "test-pool"
	const poolNamespace = "default"
	replicas := int32(1)
	template := createTemplate(poolNamespace)
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	newPool := func() *extensionsv1beta1.SandboxWarmPool {
		return &extensionsv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: poolNamespace,
				UID:       "warmpool-uid-1215",
			},
			Spec: extensionsv1beta1.SandboxWarmPoolSpec{
				Replicas:    &replicas,
				TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
			},
		}
	}

	agedSandbox := func(suffix string) *sandboxv1beta1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
		sb.UID = types.UID("uid" + suffix)
		sb.CreationTimestamp = metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		sb.Status.Conditions = []metav1.Condition{{
			Type:   string(sandboxv1beta1.SandboxConditionReady),
			Status: metav1.ConditionFalse,
		}}
		controller := true
		sb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
			Name:       poolName,
			UID:        "warmpool-uid-1215",
			Controller: &controller,
		}}
		return sb
	}

	podWithScheduled := func(name string, status corev1.ConditionStatus, reason string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: poolNamespace},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: status,
					Reason: reason,
				}},
			},
		}
	}

	t.Run("unschedulable sandbox is held, not replaced", func(t *testing.T) {
		warmPool := newPool()
		sb := agedSandbox("-unsched")
		pod := podWithScheduled(sb.Name, corev1.ConditionFalse, corev1.PodReasonUnschedulable)

		recorder := events.NewFakeRecorder(16)
		lc := newLaggingClient(newFakeClient(newTestScheme(), template, warmPool, sb, pod))
		r := SandboxWarmPoolReconciler{
			Client:       lc,
			Scheme:       newTestScheme(),
			MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
			Recorder:     recorder,
		}
		ctx := context.Background()

		requeueAfter, err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)

		// Held: still present, no replacement created, rate-limited requeue.
		got := &sandboxv1beta1.Sandbox{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: sb.Name}, got))
		require.Equal(t, 0, lc.createCount(), "no replacement may be created for an unschedulable sandbox")
		require.Equal(t, unschedulableRequeueDelay, requeueAfter)
		// It still counts as a (non-ready) replica.
		require.Equal(t, replicas, warmPool.Status.Replicas)
		require.Equal(t, int32(0), warmPool.Status.ReadyReplicas)

		// Not-progressing surfaced as a Warning event, exactly once across
		// repeated reconciles in the same state.
		select {
		case e := <-recorder.Events:
			require.Contains(t, e, reasonWarmPoolNotProgressing)
			require.Contains(t, e, corev1.EventTypeWarning)
		default:
			t.Fatal("expected a WarmPoolNotProgressing event")
		}
		_, err = r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
		select {
		case e := <-recorder.Events:
			t.Fatalf("unexpected duplicate event while state is unchanged: %s", e)
		default:
		}

		// Capacity frees up: the pod schedules and the sandbox goes Ready.
		// The hold clears and a WarmPoolProgressing event is emitted.
		got.Status.Conditions = []metav1.Condition{{
			Type:   string(sandboxv1beta1.SandboxConditionReady),
			Status: metav1.ConditionTrue,
		}}
		// Plain Update: the fake client only registers a status subresource
		// for SandboxWarmPool, so sandbox status is part of the main object.
		require.NoError(t, r.Update(ctx, got))
		requeueAfter, err = r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
		require.Zero(t, requeueAfter)
		require.Equal(t, replicas, warmPool.Status.ReadyReplicas)
		select {
		case e := <-recorder.Events:
			require.Contains(t, e, reasonWarmPoolProgressing)
			require.Contains(t, e, corev1.EventTypeNormal)
		default:
			t.Fatal("expected a WarmPoolProgressing event once progress resumes")
		}
	})

	t.Run("genuinely stuck sandbox (pod scheduled) is still replaced", func(t *testing.T) {
		warmPool := newPool()
		sb := agedSandbox("-stuck")
		// The pod scheduled fine; the sandbox is stuck for some other reason.
		pod := podWithScheduled(sb.Name, corev1.ConditionTrue, "")

		lc := newLaggingClient(newFakeClient(newTestScheme(), template, warmPool, sb, pod))
		r := SandboxWarmPoolReconciler{
			Client:       lc,
			Scheme:       newTestScheme(),
			MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
		}
		ctx := context.Background()

		// Pass 1 deletes the stuck sandbox; pass 2 (deletion observed by the
		// watch) creates the replacement.
		_, err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
		err = r.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: sb.Name}, &sandboxv1beta1.Sandbox{})
		require.True(t, client.IgnoreNotFound(err) == nil && err != nil, "stuck sandbox should be deleted")

		h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
		h.Delete(ctx, event.DeleteEvent{Object: sb}, nil)

		_, err = r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
		require.Equal(t, 1, lc.createCount(), "stuck (non-unschedulable) sandbox is replaced")
	})

	t.Run("stuck sandbox with missing pod is still replaced", func(t *testing.T) {
		warmPool := newPool()
		sb := agedSandbox("-nopod")

		lc := newLaggingClient(newFakeClient(newTestScheme(), template, warmPool, sb))
		r := SandboxWarmPoolReconciler{
			Client:       lc,
			Scheme:       newTestScheme(),
			MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
		}
		ctx := context.Background()

		_, err := r.reconcilePool(ctx, warmPool)
		require.NoError(t, err)
		err = r.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: sb.Name}, &sandboxv1beta1.Sandbox{})
		require.True(t, client.IgnoreNotFound(err) == nil && err != nil, "sandbox without a pod should be deleted")
	})
}

// phantomListClient wraps the fake client to model the opposite staleness of
// laggingClient: List still reports a sandbox that is ALREADY GONE from the
// backing store (its delete watch event fired, but the reconciler's List ran
// against a snapshot that predates it). Deleting a phantom yields NotFound.
type phantomListClient struct {
	client.WithWatch
	phantoms []sandboxv1beta1.Sandbox
}

func (c *phantomListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.WithWatch.List(ctx, list, opts...); err != nil {
		return err
	}
	if sl, ok := list.(*sandboxv1beta1.SandboxList); ok {
		sl.Items = append(sl.Items, c.phantoms...)
	}
	return nil
}

// TestReconcilePool_ExcessDeleteNotFoundLowersExpectation: an excess delete
// that hits NotFound (the sandbox vanished between our stale List and the
// Delete call — its delete watch event has already been processed, so no
// future event will arrive) must lower its deletion expectation immediately.
// Without the synthetic observation the pool would stay blocked from creating
// or deleting for up to expectationsTimeout (5 minutes).
func TestReconcilePool_ExcessDeleteNotFoundLowersExpectation(t *testing.T) {
	const poolName = "test-pool"
	const poolNamespace = "default"
	replicas := int32(3)
	template := createTemplate(poolNamespace)
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-1215",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	poolKey := types.NamespacedName{Namespace: poolNamespace, Name: poolName}

	controller := true
	newSandbox := func(suffix string, ready metav1.ConditionStatus, age time.Duration) *sandboxv1beta1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
		sb.UID = types.UID("uid" + suffix)
		// Pre-set the launch-type label so the reconcile does not try to
		// backfill it with an Update (which would NotFound on the phantom
		// before the delete path is ever reached).
		sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
		sb.CreationTimestamp = metav1.Time{Time: time.Now().Add(-age)}
		sb.Status.Conditions = []metav1.Condition{{
			Type:   string(sandboxv1beta1.SandboxConditionReady),
			Status: ready,
		}}
		sb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
			Name:       poolName,
			UID:        warmPool.UID,
			Controller: &controller,
		}}
		return sb
	}

	// Three healthy Ready sandboxes exist for real. A fourth — not Ready and
	// newest, so the excess-delete victim sort picks it first — appears only
	// in List: it is already gone from the store, so deleting it → NotFound.
	real1 := newSandbox("-a", metav1.ConditionTrue, time.Minute)
	real2 := newSandbox("-b", metav1.ConditionTrue, time.Minute)
	real3 := newSandbox("-c", metav1.ConditionTrue, time.Minute)
	phantom := newSandbox("-phantom", metav1.ConditionFalse, time.Second)

	scheme := newTestScheme()
	pc := &phantomListClient{
		WithWatch: newFakeClient(scheme, template, warmPool, real1, real2, real3),
		phantoms:  []sandboxv1beta1.Sandbox{*phantom},
	}
	r := SandboxWarmPoolReconciler{
		Client:       pc,
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
	}
	ctx := context.Background()

	// The reconcile sees 4 > 3, picks the phantom (unready first), and its
	// Delete returns NotFound.
	_, err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err, "a NotFound excess delete is not an error")

	// The deletion expectation must be lowered synchronously: no watch event
	// will ever arrive for the phantom.
	require.True(t, r.exp().SatisfiedExpectations(poolKey),
		"NotFound delete must lower its expectation immediately, not wait for the 5m timeout")
	require.False(t, r.exp().IsPendingDeletion(poolKey, phantom.UID))

	// All real sandboxes survived.
	for _, name := range []string{real1.Name, real2.Name, real3.Name} {
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: name}, &sandboxv1beta1.Sandbox{}))
	}

	// And the pool is immediately unblocked: with the phantom gone from List
	// (cache caught up) and replicas raised, the next reconcile creates
	// right away instead of being expectation-blocked.
	pc.phantoms = nil
	newReplicas := int32(4)
	warmPool.Spec.Replicas = &newReplicas
	_, err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, pc.List(ctx, list, client.InNamespace(poolNamespace)))
	require.Len(t, list.Items, 4, "next reconcile must not be blocked by the phantom's stale expectation")
}

// zeroGraceJitter disables the grace-requeue jitter for the duration of a
// test so fake-clock assertions can be exact; restored on cleanup.
func zeroGraceJitter(t *testing.T) {
	t.Helper()
	prev := graceRequeueJitterFactor
	graceRequeueJitterFactor = 0
	t.Cleanup(func() { graceRequeueJitterFactor = prev })
}

// TestReconcilePool_YoungNotReadyArmsGraceRequeue: a reconcile that observes
// not-yet-Ready sandboxes still inside the readiness grace period must
// self-schedule the post-grace evaluation (RequeueAfter = time until the
// EARLIEST grace deadline, plus slack). Without this, a pool that settles at
// Ready=False in a quiet cluster gets no further reconciles until the ~10h
// resync (pod FailedScheduling events never touch Sandbox objects), so
// neither the stuck-sandbox GC nor the unschedulable-hold/NotProgressing
// signal is ever reached — a latent reliability gap in the upstream stuck-GC
// as well.
func TestReconcilePool_YoungNotReadyArmsGraceRequeue(t *testing.T) {
	zeroGraceJitter(t)
	const poolName = "test-pool"
	const poolNamespace = "default"
	replicas := int32(3)
	template := createTemplate(poolNamespace)
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-1215",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}

	// Whole-second base: metav1.Time truncates to seconds on storage round-trips.
	base := time.Now().Truncate(time.Second)
	controller := true
	newSandbox := func(suffix string, age time.Duration, ready metav1.ConditionStatus) *sandboxv1beta1.Sandbox {
		sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, suffix)
		sb.UID = types.UID("uid" + suffix)
		sb.CreationTimestamp = metav1.Time{Time: base.Add(-age)}
		sb.Status.Conditions = []metav1.Condition{{
			Type:   string(sandboxv1beta1.SandboxConditionReady),
			Status: ready,
		}}
		sb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
			Name:       poolName,
			UID:        warmPool.UID,
			Controller: &controller,
		}}
		return sb
	}

	scheme := newTestScheme()
	r := SandboxWarmPoolReconciler{
		Client: newFakeClient(scheme,
			template, warmPool,
			newSandbox("-ready", 10*time.Minute, metav1.ConditionTrue),
			newSandbox("-young2m", 2*time.Minute, metav1.ConditionFalse),
			newSandbox("-young4m", 4*time.Minute, metav1.ConditionFalse),
		),
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
		now:          func() time.Time { return base },
	}
	ctx := context.Background()

	requeueAfter, err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	// Earliest deadline wins: the 4-minute-old sandbox has 1 minute of grace
	// left. The fake clock makes this exact.
	require.Equal(t, warmPoolReadinessGracePeriod-4*time.Minute+graceRequeueSlack, requeueAfter,
		"requeue must target the earliest remaining grace deadline")

	// With the default jitter factor, the requeue is spread inside
	// [base, base*(1+factor)] — never earlier than the deadline, never more
	// than 50% beyond it.
	graceRequeueJitterFactor = 0.5
	jitterBase := warmPoolReadinessGracePeriod - 4*time.Minute + graceRequeueSlack
	for range 20 {
		rj := SandboxWarmPoolReconciler{
			Client: newFakeClient(scheme,
				template, warmPool,
				newSandbox("-jitter", 4*time.Minute, metav1.ConditionFalse),
			),
			Scheme:       scheme,
			MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
			now:          func() time.Time { return base },
		}
		got, jerr := rj.reconcilePool(ctx, warmPool)
		require.NoError(t, jerr)
		require.GreaterOrEqual(t, got, jitterBase, "jitter must never fire before the grace deadline")
		require.LessOrEqual(t, got, time.Duration(float64(jitterBase)*(1+graceRequeueJitterFactor)),
			"jitter must be bounded by the configured factor")
	}
	graceRequeueJitterFactor = 0

	// A fully Ready pool arms no grace requeue.
	rReady := SandboxWarmPoolReconciler{
		Client: newFakeClient(scheme,
			template, warmPool,
			newSandbox("-r1", 10*time.Minute, metav1.ConditionTrue),
			newSandbox("-r2", 10*time.Minute, metav1.ConditionTrue),
			newSandbox("-r3", 10*time.Minute, metav1.ConditionTrue),
		),
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
		now:          func() time.Time { return base },
	}
	requeueAfter, err = rReady.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	require.Zero(t, requeueAfter, "a settled Ready pool must not self-requeue")
}

// TestReconcilePool_QuietClusterSelfScheduledGraceEvaluation walks the exact
// quiet-cluster sequence end to end on a fake clock, with NO external events
// between reconciles: the pool's only sandbox sits at Ready=False with an
// unschedulable pod. Reconcile 1 (inside the grace period) arms the
// self-scheduled requeue; reconcile 2 — modeling that requeue firing, nothing
// else having touched the pool — crosses the grace deadline and must apply
// the unschedulable hold and emit exactly one WarmPoolNotProgressing event.
func TestReconcilePool_QuietClusterSelfScheduledGraceEvaluation(t *testing.T) {
	zeroGraceJitter(t)
	const poolName = "test-pool"
	const poolNamespace = "default"
	replicas := int32(1)
	template := createTemplate(poolNamespace)
	poolNameHash := sandboxcontrollers.NameHash(poolName)

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: poolNamespace,
			UID:       "warmpool-uid-1215",
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}

	// Whole-second base: metav1.Time truncates to seconds on storage round-trips.
	base := time.Now().Truncate(time.Second)
	controller := true
	sb := createPoolSandbox(poolName, poolNamespace, poolNameHash, template, "-quiet")
	sb.UID = "uid-quiet"
	sb.CreationTimestamp = metav1.Time{Time: base}
	sb.Status.Conditions = []metav1.Condition{{
		Type:   string(sandboxv1beta1.SandboxConditionReady),
		Status: metav1.ConditionFalse,
	}}
	sb.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       "SandboxWarmPool",
		Name:       poolName,
		UID:        warmPool.UID,
		Controller: &controller,
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sb.Name, Namespace: poolNamespace},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable,
			}},
		},
	}

	current := base
	recorder := events.NewFakeRecorder(16)
	scheme := newTestScheme()
	lc := newLaggingClient(newFakeClient(scheme, template, warmPool, sb, pod))
	r := SandboxWarmPoolReconciler{
		Client:       lc,
		Scheme:       scheme,
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
		Recorder:     recorder,
		now:          func() time.Time { return current },
	}
	ctx := context.Background()

	// Reconcile 1: inside the grace period. No hold, no event yet — but the
	// post-grace evaluation is self-scheduled.
	requeueAfter, err := r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	require.Equal(t, warmPoolReadinessGracePeriod+graceRequeueSlack, requeueAfter)
	select {
	case e := <-recorder.Events:
		t.Fatalf("no event may fire inside the grace period, got: %s", e)
	default:
	}

	// The self-scheduled requeue fires (nothing else touched the pool).
	current = current.Add(requeueAfter)
	requeueAfter, err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)

	// Past grace: unschedulable hold applies (sandbox kept, no replacement)
	// and exactly one WarmPoolNotProgressing warning is emitted.
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: sb.Name}, &sandboxv1beta1.Sandbox{}))
	require.Equal(t, 0, lc.createCount())
	require.Equal(t, unschedulableRequeueDelay, requeueAfter)
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, reasonWarmPoolNotProgressing)
		require.Contains(t, e, corev1.EventTypeWarning)
	default:
		t.Fatal("expected WarmPoolNotProgressing after the self-scheduled post-grace reconcile")
	}

	// The rate-limited hold requeue fires again: still held, no duplicate.
	current = current.Add(requeueAfter)
	_, err = r.reconcilePool(ctx, warmPool)
	require.NoError(t, err)
	select {
	case e := <-recorder.Events:
		t.Fatalf("unexpected duplicate event while state is unchanged: %s", e)
	default:
	}
}

// TestWarmPoolSandboxEventHandler_ObservesOwnedEvents covers the watch-side
// bookkeeping: add/delete events for pool-owned sandboxes lower the matching
// expectations; unowned or foreign-owned objects are ignored.
func TestWarmPoolSandboxEventHandler_ObservesOwnedEvents(t *testing.T) {
	e := newWarmPoolExpectations()
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: e}
	ctx := context.Background()
	poolKey := types.NamespacedName{Namespace: "default", Name: "test-pool"}

	controller := true
	owned := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:      "owned",
		Namespace: "default",
		UID:       "uid-owned",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: extensionsv1beta1.GroupVersion.String(),
			Kind:       "SandboxWarmPool",
			Name:       poolKey.Name,
			UID:        "warmpool-uid",
			Controller: &controller,
		}},
	}}
	orphan := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "orphan", Namespace: "default", UID: "uid-orphan",
	}}
	foreign := owned.DeepCopy()
	foreign.OwnerReferences[0].Kind = "ReplicaSet"
	foreign.OwnerReferences[0].APIVersion = "apps/v1"

	require.True(t, e.TryExpectCreations(poolKey, 1))
	h.Create(ctx, event.CreateEvent{Object: orphan}, nil)
	h.Create(ctx, event.CreateEvent{Object: foreign}, nil)
	require.False(t, e.SatisfiedExpectations(poolKey), "unowned/foreign adds must not lower expectations")
	h.Create(ctx, event.CreateEvent{Object: owned}, nil)
	require.True(t, e.SatisfiedExpectations(poolKey))

	e.ExpectDeletion(poolKey, owned.UID)
	h.Delete(ctx, event.DeleteEvent{Object: orphan}, nil)
	require.False(t, e.SatisfiedExpectations(poolKey))
	h.Delete(ctx, event.DeleteEvent{Object: owned}, nil)
	require.True(t, e.SatisfiedExpectations(poolKey))
}
