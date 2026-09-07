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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

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
