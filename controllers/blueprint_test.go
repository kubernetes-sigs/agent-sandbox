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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// blueprintTestScheme mirrors the production scheme with extensions enabled
// (main.go registers extensionsv1beta1 when --extensions=true), which is the
// only configuration that produces blueprintRef sandboxes.
func blueprintTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sandboxv1beta1.AddToScheme(scheme))
	utilruntime.Must(extensionsv1beta1.AddToScheme(scheme))
	return scheme
}

func newBlueprintFakeClient(t *testing.T, scheme *runtime.Scheme, initialObjs ...runtime.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithRuntimeObjects(initialObjs...).
		Build()
}

func blueprintTestTemplate(t *testing.T) (*extensionsv1beta1.SandboxTemplate, string) {
	t.Helper()
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"template-label": "yes"},
					},
					Spec: &corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "main",
							Image: "test-image",
							Ports: []corev1.ContainerPort{{ContainerPort: 8000}},
						}},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{{
					EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				}},
				Service: new(true),
			},
		},
	}
	hash, err := BlueprintHash(&template.Spec.SandboxBlueprint)
	if err != nil {
		t.Fatalf("BlueprintHash failed: %v", err)
	}
	return template, hash
}

func blueprintRefSandbox(hash string) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ref-sandbox",
			Namespace: "default",
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"inline-label": "yes"},
					},
				},
			},
			BlueprintRef: &sandboxv1beta1.BlueprintRef{
				Name:          "test-template",
				BlueprintHash: hash,
			},
		},
	}
}

func TestResolveBlueprintRef__HydratesBlueprintWithSecureDefaults(t *testing.T) {
	scheme := blueprintTestScheme(t)
	template, hash := blueprintTestTemplate(t)
	sandbox := blueprintRefSandbox(hash)
	r := &SandboxReconciler{
		Client: newBlueprintFakeClient(t, scheme, template),
		Scheme: scheme,
	}

	if err := r.resolveBlueprintRef(t.Context(), sandbox); err != nil {
		t.Fatalf("resolveBlueprintRef failed: %v", err)
	}

	spec := sandbox.Spec.PodTemplate.Spec
	if spec == nil {
		t.Fatal("expected pod spec to be hydrated")
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Image != "test-image" {
		t.Errorf("unexpected hydrated containers: %+v", spec.Containers)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("expected secure default AutomountServiceAccountToken=false")
	}
	if spec.DNSPolicy != corev1.DNSNone {
		t.Errorf("expected secure default DNSPolicy=None, got %q", spec.DNSPolicy)
	}
	if len(sandbox.Spec.VolumeClaimTemplates) != 1 || sandbox.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Errorf("expected volumeClaimTemplates hydrated from template, got %+v", sandbox.Spec.VolumeClaimTemplates)
	}
	if sandbox.Spec.Service == nil || !*sandbox.Spec.Service {
		t.Error("expected service flag hydrated from template")
	}
	// The inline pod template metadata is the adoption channel and must never
	// be replaced by the template's copy.
	if sandbox.Spec.PodTemplate.ObjectMeta.Labels["inline-label"] != "yes" {
		t.Error("expected inline podTemplate metadata to be preserved")
	}

	// Hydration must be a deep copy: mutating the hydrated spec must not
	// reach the (cached) template object.
	spec.Containers[0].Image = "mutated"
	if template.Spec.PodTemplate.Spec.Containers[0].Image != "test-image" {
		t.Error("hydrated spec aliases the template's pod spec")
	}
}

func TestResolveBlueprintRef__NoopWithoutRef(t *testing.T) {
	scheme := blueprintTestScheme(t)
	sandbox := &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: &corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
				},
			},
		},
	}
	r := &SandboxReconciler{Client: newBlueprintFakeClient(t, scheme), Scheme: scheme}
	if err := r.resolveBlueprintRef(t.Context(), sandbox); err != nil {
		t.Fatalf("expected no-op for embedded sandbox, got %v", err)
	}
}

func TestResolveBlueprintRef__TemplateNotFound(t *testing.T) {
	scheme := blueprintTestScheme(t)
	sandbox := blueprintRefSandbox("some-hash")
	r := &SandboxReconciler{Client: newBlueprintFakeClient(t, scheme), Scheme: scheme}

	err := r.resolveBlueprintRef(t.Context(), sandbox)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found resolution error, got %v", err)
	}
	if sandbox.Spec.PodTemplate.Spec != nil {
		t.Error("expected pod spec to stay unresolved on failure")
	}
}

func TestResolveBlueprintRef__HashMismatch(t *testing.T) {
	scheme := blueprintTestScheme(t)
	template, _ := blueprintTestTemplate(t)
	sandbox := blueprintRefSandbox("stale-hash")
	r := &SandboxReconciler{Client: newBlueprintFakeClient(t, scheme, template), Scheme: scheme}

	err := r.resolveBlueprintRef(t.Context(), sandbox)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected hash-mismatch resolution error, got %v", err)
	}
	if sandbox.Spec.PodTemplate.Spec != nil {
		t.Error("expected pod spec to stay unresolved on failure")
	}
}

func TestReconcile__BlueprintRefSandbox_CreatesPodAndChildren(t *testing.T) {
	scheme := blueprintTestScheme(t)
	template, hash := blueprintTestTemplate(t)
	sandbox := blueprintRefSandbox(hash)
	c := newBlueprintFakeClient(t, scheme, template, sandbox)
	r := &SandboxReconciler{Client: c, Scheme: scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	pod := &corev1.Pod{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}, pod); err != nil {
		t.Fatalf("expected backing pod to be created: %v", err)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "test-image" {
		t.Errorf("unexpected pod containers: %+v", pod.Spec.Containers)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("expected pod to carry secure default AutomountServiceAccountToken=false")
	}
	// Inline pod template metadata still propagates to the pod.
	if pod.Labels["inline-label"] != "yes" {
		t.Errorf("expected inline pod template label on pod, got %v", pod.Labels)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "data-ref-sandbox", Namespace: "default"}, pvc); err != nil {
		t.Fatalf("expected PVC from hydrated volumeClaimTemplates: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}, svc); err != nil {
		t.Fatalf("expected Service from hydrated service flag: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8000 {
		t.Errorf("expected service port derived from hydrated containers, got %+v", svc.Spec.Ports)
	}

	// The hydrated blueprint must never be persisted back to the Sandbox.
	stored := &sandboxv1beta1.Sandbox{}
	if err := c.Get(t.Context(), req.NamespacedName, stored); err != nil {
		t.Fatalf("failed to re-read sandbox: %v", err)
	}
	if stored.Spec.PodTemplate.Spec != nil || stored.Spec.VolumeClaimTemplates != nil || stored.Spec.Service != nil {
		t.Errorf("hydrated blueprint leaked into the stored Sandbox: %+v", stored.Spec)
	}
}

func TestReconcile__BlueprintRefUnresolved_NoPod_SurfacesError(t *testing.T) {
	scheme := blueprintTestScheme(t)
	sandbox := blueprintRefSandbox("some-hash")
	c := newBlueprintFakeClient(t, scheme, sandbox)
	r := &SandboxReconciler{Client: c, Scheme: scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}}
	if _, err := r.Reconcile(t.Context(), req); err == nil {
		t.Fatal("expected reconcile error while the blueprint is unresolved and no pod exists")
	}

	pod := &corev1.Pod{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}, pod); !k8serrors.IsNotFound(err) {
		t.Fatalf("expected no pod to be created, got err=%v", err)
	}

	stored := &sandboxv1beta1.Sandbox{}
	if err := c.Get(t.Context(), req.NamespacedName, stored); err != nil {
		t.Fatalf("failed to re-read sandbox: %v", err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False while unresolved with no pod, got %+v", ready)
	}
}

func TestReconcile__BlueprintRefUnresolved_LivePod_StaysReadyAndUntouched(t *testing.T) {
	scheme := blueprintTestScheme(t)
	template, hash := blueprintTestTemplate(t)
	sandbox := blueprintRefSandbox(hash)
	nameHash := NameHash(sandbox.Name)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ref-sandbox",
			Namespace:       "default",
			Labels:          map[string]string{sandboxLabel: nameHash, "inline-label": "yes"},
			Annotations:     map[string]string{sandboxv1beta1.SandboxPropagatedLabelsAnnotation: "inline-label"},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef("ref-sandbox")},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test-image"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			PodIPs:     []corev1.PodIP{{IP: "10.0.0.1"}},
		},
	}
	// The template exists at first so we can verify the baseline, then is
	// deleted to simulate drift/removal under a live workload.
	c := newBlueprintFakeClient(t, scheme, template, sandbox, pod)
	r := &SandboxReconciler{Client: c, Scheme: scheme, Tracer: asmetrics.NewNoOp(), ClusterDomain: "cluster.local"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}}

	if err := c.Delete(t.Context(), template); err != nil {
		t.Fatalf("failed to delete template: %v", err)
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("expected no reconcile error for an unresolved blueprint with a live pod, got %v", err)
	}

	stored := &sandboxv1beta1.Sandbox{}
	if err := c.Get(t.Context(), req.NamespacedName, stored); err != nil {
		t.Fatalf("failed to re-read sandbox: %v", err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True from the live pod despite unresolved blueprint, got %+v", ready)
	}
	if len(stored.Status.PodIPs) != 1 || stored.Status.PodIPs[0] != "10.0.0.1" {
		t.Errorf("expected pod IPs preserved, got %v", stored.Status.PodIPs)
	}

	livePod := &corev1.Pod{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "ref-sandbox", Namespace: "default"}, livePod); err != nil {
		t.Fatalf("expected the live pod to survive: %v", err)
	}
	if !livePod.DeletionTimestamp.IsZero() {
		t.Error("expected the live pod not to be deleted")
	}
}
