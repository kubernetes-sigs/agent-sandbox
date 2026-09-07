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

package runtimeadoption

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestProtectedStatusCreatesAbsentParentAndPreservesOtherWriters(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1beta1.AddToScheme(scheme))
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "agents", UID: "sandbox-uid"}}
	writer := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).WithStatusSubresource(sandbox).Build()
	require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
	data, err := json.Marshal(sandbox)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	require.NotContains(t, wire, "status")
	status := &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "attempt-0000000001"}}
	require.NoError(t, PatchStatus(ctx, writer, sandbox, status))
	require.Equal(t, status, sandbox.Status.RuntimeAdoption)
	stale := sandbox.DeepCopy()
	sandbox.Status.ServiceFQDN = "sandbox.agents.svc.cluster.local"
	sandbox.Status.RuntimeActivationVerification = &sandboxv1beta1.RuntimeActivationVerification{AttemptID: "attempt-0000000001"}
	require.NoError(t, writer.Status().Update(ctx, sandbox))
	status.HoldEvidenceDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err = PatchStatus(ctx, writer, stale, status)
	require.True(t, apierrors.IsConflict(err), "stale protected status must conflict: %v", err)
	require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(sandbox), stale))
	require.NoError(t, PatchStatus(ctx, writer, stale, status))
	require.Equal(t, sandbox.Status.ServiceFQDN, stale.Status.ServiceFQDN)
	require.Equal(t, sandbox.Status.RuntimeActivationVerification, stale.Status.RuntimeActivationVerification)
	require.Equal(t, status, stale.Status.RuntimeAdoption)
	stale.UID = ""
	require.Error(t, PatchStatus(ctx, writer, stale, status))
}

func TestFinalizerRemovalRejectsConcurrentReservation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, sandboxv1beta1.AddToScheme(scheme))
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "agents", UID: "sandbox-uid", Finalizers: []string{sandboxv1beta1.RuntimeAdoptionFinalizer}}}
	writer := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).WithStatusSubresource(sandbox).Build()
	require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
	stale := sandbox.DeepCopy()
	require.NoError(t, PatchStatus(ctx, writer, sandbox, &sandboxv1beta1.RuntimeAdoptionStatus{Reservation: &sandboxv1beta1.RuntimeAdoptionReservation{AttemptID: "winning-attempt-0001"}}))
	require.Error(t, SetFinalizer(ctx, writer, stale, false))
	require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
	require.Contains(t, sandbox.Finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
	require.Equal(t, sandboxv1beta1.RuntimeAdoptionID("winning-attempt-0001"), sandbox.Status.RuntimeAdoption.Reservation.AttemptID)
}
