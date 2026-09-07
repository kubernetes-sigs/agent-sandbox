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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func committedRuntimeAdoptionFixture() (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	const contextDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const receiptDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const commitDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const grantDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	claim := &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"}}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: claim.Namespace, UID: "sandbox-uid", Generation: 1,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))}},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{Service: new(false)}},
		Status: sandboxv1beta1.SandboxStatus{
			RuntimeAdoption: &sandboxv1beta1.RuntimeAdoptionStatus{
				Initialization: &sandboxv1beta1.RuntimeAdoptionInitialization{},
				Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{
					AttemptID: "attempt-0000000001", TargetActivationID: "target-0000000001", Namespace: claim.Namespace,
					SandboxUID: "sandbox-uid", ClaimName: claim.Name, ClaimUID: claim.UID,
					ExpiresAt: metav1.NewTime(time.Now().Add(-time.Hour)),
				},
				Grant: &sandboxv1beta1.RuntimeAdoptionGrant{ContextDigest: contextDigest, ReceiptDigest: receiptDigest,
					GrantDigest: grantDigest, ExpiresAt: metav1.NewTime(time.Now().Add(-time.Hour))},
				CommitDigest: commitDigest,
			},
			RuntimeActivationVerification: &sandboxv1beta1.RuntimeActivationVerification{
				AttemptID: "attempt-0000000001", ClaimUID: claim.UID, TargetActivationID: "target-0000000001",
				PodUID: "pod-uid", NodeUID: "node-uid", ContainerID: "container-id", TaskStartTime: 42, RuntimeIncarnation: "runtime-incarnation",
				ReceiptDigest: receiptDigest, ContextDigest: contextDigest, CommitDigest: commitDigest,
				ValidUntil: metav1.NewTime(time.Now().Add(time.Minute)), Conditions: []metav1.Condition{{Type: "Verified", Status: metav1.ConditionTrue}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace, UID: "pod-uid",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sandbox, sandboxv1beta1.GroupVersion.WithKind("Sandbox"))}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://container-id", Ready: true}},
		},
	}
	return sandbox, pod
}

func TestRuntimeAdoptionReadyRequiresCurrentVerification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sandboxv1beta1.Sandbox, *corev1.Pod)
		ready  bool
	}{
		{name: "current verification after historical authorization expires", ready: true},
		{name: "missing verification", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) { sb.Status.RuntimeActivationVerification = nil }},
		{name: "expired verification", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.ValidUntil = metav1.NewTime(time.Now().Add(-time.Second))
		}},
		{name: "different attempt", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.AttemptID = "attempt-0000000002"
		}},
		{name: "different claim", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.ClaimUID = "another-claim"
		}},
		{name: "different target activation", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.TargetActivationID = "target-0000000002"
		}},
		{name: "different commit", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.CommitDigest = sb.Status.RuntimeAdoption.Grant.ReceiptDigest
		}},
		{name: "different context", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.ContextDigest = sb.Status.RuntimeAdoption.Grant.ReceiptDigest
		}},
		{name: "incomplete verification", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) { sb.Status.RuntimeActivationVerification.NodeUID = "" }},
		{name: "verification withdrawn", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeActivationVerification.Conditions[0].Status = metav1.ConditionFalse
		}},
		{name: "grant incomplete", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) { sb.Status.RuntimeAdoption.Grant.GrantDigest = "" }},
		{name: "claim owner changed", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) { sb.OwnerReferences[0].UID = "another-claim" }},
		{name: "pod replaced", mutate: func(_ *sandboxv1beta1.Sandbox, pod *corev1.Pod) { pod.UID = "replacement-pod" }},
		{name: "pod owner changed", mutate: func(_ *sandboxv1beta1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences[0].UID = "another-sandbox" }},
		{name: "container replaced", mutate: func(_ *sandboxv1beta1.Sandbox, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement-container"
		}},
		{name: "termination requested", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			now := metav1.Now()
			sb.Status.RuntimeAdoption.TerminationRequestedTime = &now
		}},
		{name: "terminal observation", mutate: func(sb *sandboxv1beta1.Sandbox, _ *corev1.Pod) {
			sb.Status.RuntimeAdoption.TerminalEvidenceDigest = sb.Status.RuntimeAdoption.CommitDigest
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, pod := committedRuntimeAdoptionFixture()
			if tc.mutate != nil {
				tc.mutate(sandbox, pod)
			}
			condition := (&SandboxReconciler{}).computeReadyCondition(sandbox, nil, nil, pod)
			require.Equal(t, tc.ready, condition.Status == metav1.ConditionTrue, "%+v", condition)
			if !tc.ready {
				require.Equal(t, "RuntimeAdoptionPending", condition.Reason)
			}
		})
	}
}

func TestRuntimeAdoptionReadinessRequeuesAtVerificationExpiry(t *testing.T) {
	sandbox, pod := committedRuntimeAdoptionFixture()
	c := newFakeClient(sandbox, pod)
	r := &SandboxReconciler{Client: c, APIReader: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}
	result, err := r.Reconcile(t.Context(), request)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.LessOrEqual(t, result.RequeueAfter, time.Minute)
	require.NoError(t, c.Get(t.Context(), request.NamespacedName, sandbox))
	require.True(t, meta.IsStatusConditionTrue(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady)))

	sandbox.Status.RuntimeActivationVerification.ValidUntil = metav1.NewTime(time.Now().Add(-time.Second))
	require.NoError(t, c.Status().Update(t.Context(), sandbox))
	result, err = r.Reconcile(t.Context(), request)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.NoError(t, c.Get(t.Context(), request.NamespacedName, sandbox))
	condition := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "RuntimeAdoptionPending", condition.Reason)
}

func TestCoreStatusPreservesConcurrentRuntimeEvidence(t *testing.T) {
	ctx := context.Background()
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "sandbox-uid"}}
	c := newFakeClient(sandbox)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
	stale := sandbox.DeepCopy()
	oldStatus := stale.Status.DeepCopy()

	// A trusted writer reserves the Sandbox after the core informer snapshot.
	sandbox.Status.RuntimeAdoption = &sandboxv1beta1.RuntimeAdoptionStatus{
		Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid"},
		Consumed:    &sandboxv1beta1.RuntimeAdoptionConsumption{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid", ConsumedTime: metav1.Now()},
	}
	sandbox.Status.RuntimeActivationVerification = &sandboxv1beta1.RuntimeActivationVerification{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid"}
	require.NoError(t, c.Status().Update(ctx, sandbox))

	stale.Status.ServiceFQDN = "sandbox.default.svc.cluster.local"
	r := &SandboxReconciler{Client: c}
	require.NoError(t, r.updateStatus(ctx, oldStatus, stale))
	observed := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sandbox), observed))
	require.Equal(t, sandbox.Status.RuntimeAdoption, observed.Status.RuntimeAdoption)
	require.Equal(t, sandbox.Status.RuntimeActivationVerification, observed.Status.RuntimeActivationVerification)
	require.Equal(t, stale.Status.ServiceFQDN, observed.Status.ServiceFQDN)
}

func TestCoreExpiryPreservesRuntimeEvidence(t *testing.T) {
	ctx := context.Background()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "sandbox-uid"},
		Status: sandboxv1beta1.SandboxStatus{
			ServiceFQDN: "sandbox.default.svc.cluster.local", PodIPs: []string{"10.0.0.1"},
			RuntimeAdoption:               &sandboxv1beta1.RuntimeAdoptionStatus{Consumed: &sandboxv1beta1.RuntimeAdoptionConsumption{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid", ConsumedTime: metav1.Now()}},
			RuntimeActivationVerification: &sandboxv1beta1.RuntimeActivationVerification{AttemptID: "attempt-0000000001", ClaimUID: "claim-uid"},
		},
	}
	before := sandbox.Status.DeepCopy()
	r := &SandboxReconciler{Client: newFakeClient(sandbox)}
	deleted, err := r.handleSandboxExpiry(ctx, sandbox)
	require.NoError(t, err)
	require.False(t, deleted)
	require.Equal(t, before.RuntimeAdoption, sandbox.Status.RuntimeAdoption)
	require.Equal(t, before.RuntimeActivationVerification, sandbox.Status.RuntimeActivationVerification)
	require.Empty(t, sandbox.Status.ServiceFQDN)
	require.Empty(t, sandbox.Status.PodIPs)
}

func TestHeldPodMetadataIsWrittenBeforeAcknowledgement(t *testing.T) {
	ctx := context.Background()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "sandbox-uid"},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
			PodTemplate: sandboxv1beta1.PodTemplate{ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"team": "claim"}}},
		}},
		Status: sandboxv1beta1.SandboxStatus{RuntimeAdoption: &sandboxv1beta1.RuntimeAdoptionStatus{
			Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001"}, Initialization: &sandboxv1beta1.RuntimeAdoptionInitialization{},
			HoldEvidenceDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace, UID: "pod-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sandbox, sandboxv1beta1.GroupVersion.WithKind("Sandbox"))},
		Labels:          map[string]string{"team": "pool"},
	}}
	c := newFakeClient(sandbox, pod)
	r := &SandboxReconciler{Client: c, APIReader: c}
	deferral := &writeDeferral{clock: &deferredWriteClock{}, key: client.ObjectKeyFromObject(sandbox), window: time.Hour}
	observed, err := r.reconcilePod(ctx, sandbox, NameHash(sandbox.Name), deferral)
	require.NoError(t, err)
	require.Equal(t, "claim", observed.Labels["team"])
	require.False(t, deferral.deferred, "held metadata cannot be deferred past the grant boundary")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	require.Equal(t, "claim", pod.Labels["team"])
	require.True(t, SandboxPodMetadataMatches(ctx, sandbox, pod))

	sandbox.Status.RuntimeAdoption.TargetMetadataDigest = sandbox.Status.RuntimeAdoption.HoldEvidenceDigest
	require.NoError(t, c.Status().Update(ctx, sandbox))
	// A stale controller's next pass cannot rewrite already acknowledged data.
	sandbox.Spec.PodTemplate.ObjectMeta.Labels["team"] = "stale-pool"
	observed, err = r.reconcilePod(ctx, sandbox, NameHash(sandbox.Name), deferral)
	require.NoError(t, err)
	require.Equal(t, "claim", observed.Labels["team"])

	require.NoError(t, c.Delete(ctx, pod))
	_, err = r.reconcilePod(ctx, sandbox, NameHash(sandbox.Name), deferral)
	require.True(t, apierrors.IsNotFound(err), "missing reserved execution must remain missing: %v", err)
	var pods corev1.PodList
	require.NoError(t, c.List(ctx, &pods))
	require.Empty(t, pods.Items)
}
