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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestRecordStageLatenciesOneShot(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	observedAt := time.Now().Add(-2 * time.Second).UTC()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stage-sb",
			Namespace: "default",
			UID:       sandboxUID,
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: observedAt.Format(time.RFC3339Nano),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}

	ltt := metav1.NewTime(observedAt.Add(500 * time.Millisecond))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "stage-sb",
			Namespace:         "default",
			UID:               "pod-uid",
			CreationTimestamp: metav1.NewTime(observedAt.Add(100 * time.Millisecond)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("stage-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("stage-sb")},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
			},
			StartTime: &ltt,
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "stage-sb",
			Namespace:         "default",
			UID:               "svc-uid",
			CreationTimestamp: metav1.NewTime(observedAt.Add(200 * time.Millisecond)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("stage-sb")},
		},
	}

	c := newFakeClient(sandbox, pod, svc)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	r.recordStageLatencies(context.Background(), sandbox, pod, svc)
	require.Equal(t, 5, testutil.CollectAndCount(asmetrics.SandboxStageLatency),
		"expected pod_created, pod_scheduled, pod_running, pod_ready, service_ready")

	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updated))
	recorded := asmetrics.ParseStageLatencyRecorded(updated.Annotations[asmetrics.StageLatencyRecordedAnnotation])
	require.Contains(t, recorded, asmetrics.StagePodCreated)
	require.Contains(t, recorded, asmetrics.StagePodScheduled)
	require.Contains(t, recorded, asmetrics.StagePodRunning)
	require.Contains(t, recorded, asmetrics.StagePodReady)
	require.Contains(t, recorded, asmetrics.StageServiceReady)
	require.NotContains(t, recorded, asmetrics.StagePVCBound)

	// Second call must not double-count.
	sandbox.Annotations = updated.Annotations
	r.recordStageLatencies(context.Background(), sandbox, pod, svc)
	require.Equal(t, 5, testutil.CollectAndCount(asmetrics.SandboxStageLatency))
}

func TestRecordStageLatenciesSkipsPreObservationStages(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	// Warm launch / upgrade: children reached Ready before the controller first observed the Sandbox.
	observedAt := time.Now().UTC()
	readyAt := observedAt.Add(-5 * time.Second)
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       sandboxUID,
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: observedAt.Format(time.RFC3339Nano),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	ltt := metav1.NewTime(readyAt)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-sb",
			Namespace:         "default",
			UID:               "pod-uid",
			CreationTimestamp: metav1.NewTime(readyAt.Add(-time.Second)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("warm-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("warm-sb")},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
			},
			StartTime: &ltt,
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-sb",
			Namespace:         "default",
			UID:               "svc-uid",
			CreationTimestamp: metav1.NewTime(readyAt),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("warm-sb")},
		},
	}

	c := newFakeClient(sandbox, pod, svc)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	r.recordStageLatencies(context.Background(), sandbox, pod, svc)
	require.Equal(t, 0, testutil.CollectAndCount(asmetrics.SandboxStageLatency),
		"pre-observation stages must not emit near-zero samples")

	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updated))
	recorded := asmetrics.ParseStageLatencyRecorded(updated.Annotations[asmetrics.StageLatencyRecordedAnnotation])
	require.Contains(t, recorded, asmetrics.StagePodCreated)
	require.Contains(t, recorded, asmetrics.StagePodReady)
	require.Contains(t, recorded, asmetrics.StageServiceReady)
}

func TestRecordStageLatenciesPVCBoundUsesObservationTime(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	observedAt := time.Now().Add(-10 * time.Second).UTC()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-sb",
			Namespace: "default",
			UID:       sandboxUID,
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: observedAt.Format(time.RFC3339Nano),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec:                   corev1.PersistentVolumeClaimSpec{},
				}},
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	// CreationTimestamp is near t0; if used as bind time, latency would be ~50ms.
	// Observation time (now) yields ~10s — covered by TestPVCBoundTransitionTimeUsesFallback
	// and by asserting the stage is recorded (endTime = now > t0, so it is observed).
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "data-pvc-sb",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(observedAt.Add(50 * time.Millisecond)),
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	c := newFakeClient(sandbox, pvc)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	r.recordStageLatencies(context.Background(), sandbox, nil, nil)

	require.Equal(t, 1, testutil.CollectAndCount(asmetrics.SandboxStageLatency))
	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updated))
	require.Contains(t, asmetrics.ParseStageLatencyRecorded(updated.Annotations[asmetrics.StageLatencyRecordedAnnotation]), asmetrics.StagePVCBound)
}

func TestReconcileStampsObservabilityAndRecordsPodCreated(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "obs-sb",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	c := newFakeClient(sandbox)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}})
	require.NoError(t, err)

	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated))
	require.NotEmpty(t, updated.Annotations[asmetrics.ObservabilityAnnotation])
	require.Contains(t, asmetrics.ParseStageLatencyRecorded(updated.Annotations[asmetrics.StageLatencyRecordedAnnotation]), asmetrics.StagePodCreated)
	require.GreaterOrEqual(t, testutil.CollectAndCount(asmetrics.SandboxStageLatency), 1)
}

func TestRecordStageLatenciesBatchesObservabilityAnnotation(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "batch-sb",
			Namespace: "default",
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "batch-sb",
			Namespace:         "default",
			UID:               "pod-uid",
			CreationTimestamp: metav1.Now(),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("batch-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("batch-sb")},
		},
	}

	c := newFakeClient(sandbox, pod)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	r.recordStageLatencies(context.Background(), sandbox, pod, nil)

	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated))
	require.NotEmpty(t, updated.Annotations[asmetrics.ObservabilityAnnotation],
		"ObservabilityAnnotation should be persisted by recordStageLatencies")
	require.Contains(t, asmetrics.ParseStageLatencyRecorded(updated.Annotations[asmetrics.StageLatencyRecordedAnnotation]), asmetrics.StagePodCreated)
}

func TestRecordStageLatenciesPatchFailureDoesNotEmitMetrics(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	observedAt := time.Now().Add(-2 * time.Second).UTC()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fail-sb",
			Namespace: "default",
			UID:       sandboxUID,
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: observedAt.Format(time.RFC3339Nano),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fail-sb",
			Namespace:         "default",
			UID:               "pod-uid",
			CreationTimestamp: metav1.NewTime(observedAt.Add(100 * time.Millisecond)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("fail-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("fail-sb")},
		},
	}

	// Client without the Sandbox object: Patch will fail.
	c := newFakeClient(pod)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	r.recordStageLatencies(context.Background(), sandbox, pod, nil)
	require.Equal(t, 0, testutil.CollectAndCount(asmetrics.SandboxStageLatency),
		"metrics must not emit when annotation patch fails")
}

func TestEnsureSandboxObservabilityAnnotationsPatchFailureIsBestEffort(_ *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "obs-fail-sb",
			Namespace: "default",
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	// Client without the Sandbox: Patch fails. Must not panic or block callers.
	c := newFakeClient()
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}
	r.ensureSandboxObservabilityAnnotations(context.Background(), sandbox)
}

func TestReconcileSuspendedStampsObservabilityViaEnsure(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "obs-susp-sb",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			OperatingMode: sandboxv1beta1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	c := newFakeClient(sandbox)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace},
	})
	require.NoError(t, err)

	updated := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated))
	require.NotEmpty(t, updated.Annotations[asmetrics.ObservabilityAnnotation],
		"Suspended sandboxes stamp ObservabilityAnnotation in ensureSandboxObservabilityAnnotations")
	require.Empty(t, updated.Annotations[asmetrics.StageLatencyRecordedAnnotation],
		"stage latency is skipped while Suspended")
	require.Equal(t, 0, testutil.CollectAndCount(asmetrics.SandboxStageLatency))
}

func TestReconcileChildResourcesStageLatencyDoesNotBlockReady(t *testing.T) {
	asmetrics.SandboxStageLatency.Reset()

	observedAt := time.Now().Add(-2 * time.Second).UTC()
	ltt := metav1.NewTime(observedAt.Add(500 * time.Millisecond))
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ready-sb",
			Namespace:  "default",
			UID:        sandboxUID,
			Generation: 1,
			Annotations: map[string]string{
				asmetrics.ObservabilityAnnotation: observedAt.Format(time.RFC3339Nano),
			},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ready-sb",
			Namespace:         "default",
			UID:               "pod-uid",
			CreationTimestamp: metav1.NewTime(observedAt.Add(100 * time.Millisecond)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("ready-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("ready-sb")},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ltt},
			},
			StartTime: &ltt,
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ready-sb",
			Namespace:         "default",
			UID:               "svc-uid",
			CreationTimestamp: metav1.NewTime(observedAt.Add(200 * time.Millisecond)),
			OwnerReferences:   []metav1.OwnerReference{sandboxControllerRef("ready-sb")},
			Labels:            map[string]string{sandboxLabel: NameHash("ready-sb")},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "None"},
	}

	c := newFakeClient(sandbox, pod, svc)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}

	err := r.reconcileChildResources(context.Background(), sandbox)
	require.NoError(t, err, "stage-latency bookkeeping must not fail reconcile")

	ready := false
	for _, cond := range sandbox.Status.Conditions {
		if cond.Type == string(sandboxv1beta1.SandboxConditionReady) {
			ready = cond.Status == metav1.ConditionTrue
			require.NotEqual(t, "ReconcilerError", cond.Reason)
		}
	}
	require.True(t, ready, "healthy sandbox must remain Ready when only telemetry patches are involved")
}

func TestRecordChildReconcileErrorOnOwnershipConflict(t *testing.T) {
	asmetrics.ChildReconcileErrors.Reset()

	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "own-sb",
			Namespace: "default",
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img"}},
					},
				},
			},
		},
	}
	otherOwner := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "own-sb",
			Namespace: "default",
			UID:       "pod-uid",
			Labels:    map[string]string{sandboxLabel: NameHash("own-sb")},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "rs",
				UID:        "rs-uid",
				Controller: &otherOwner,
			}},
		},
	}
	c := newFakeClient(sandbox, pod)
	r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

	_, err := r.reconcilePod(context.Background(), sandbox, NameHash(sandbox.Name))
	require.Error(t, err)
	require.Equal(t, 1, testutil.CollectAndCount(asmetrics.ChildReconcileErrors))
}

func TestPVCBoundTransitionTimeUsesFallback(t *testing.T) {
	fallback := time.Now()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(fallback.Add(-5 * time.Minute)),
		},
	}
	got := pvcBoundTransitionTime(pvc, fallback)
	require.Equal(t, fallback, got)
	require.Equal(t, fallback, pvcBoundTransitionTime(nil, fallback))
}
