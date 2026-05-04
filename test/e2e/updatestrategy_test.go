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

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestUpdateStrategyDefaultNoResize(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-default-noresize-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Sandbox with no UpdateStrategy set (nil) — default should NOT resize
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-noresize-sandbox",
			Namespace: ns.Name,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			// UpdateStrategy intentionally nil
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

	nameHash := NameHash(sandbox.Name)
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "default-noresize-sandbox",
			ServiceFQDN:   fmt.Sprintf("default-noresize-sandbox.%s.svc.cluster.local", ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Message:            "Pod is Ready; Service Exists",
					ObservedGeneration: 1,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandbox, p...)

	pod := &corev1.Pod{}
	pod.Name = sandbox.Name
	pod.Namespace = ns.Name
	tc.MustExist(pod)
	initialCPU := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	initialMem := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]

	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	})

	require.Eventually(t, func() bool {
		current := &sandboxv1alpha1.Sandbox{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, current); err != nil {
			return false
		}
		for _, cond := range current.Status.Conditions {
			if cond.Type == "Ready" && cond.ObservedGeneration == current.Generation {
				return true
			}
		}
		return false
	}, 30*time.Second, time.Second)

	freshPod := &corev1.Pod{}
	freshPod.Name = sandbox.Name
	freshPod.Namespace = ns.Name
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}, freshPod))
	require.Equal(t, initialCPU, freshPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU],
		"pod CPU request should not have changed with default (nil) UpdateStrategy")
	require.Equal(t, initialMem, freshPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory],
		"pod memory request should not have changed with default (nil) UpdateStrategy")
}

func TestUpdateStrategyNoResize(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-noresize-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noresize-sandbox",
			Namespace: ns.Name,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			UpdateStrategy: &sandboxv1alpha1.SandboxUpdateStrategy{
				Type: sandboxv1alpha1.NoneSandboxUpdateStrategyType,
			},
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

	nameHash := NameHash(sandbox.Name)
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "noresize-sandbox",
			ServiceFQDN:   fmt.Sprintf("noresize-sandbox.%s.svc.cluster.local", ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Message:            "Pod is Ready; Service Exists",
					ObservedGeneration: 1,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandbox, p...)

	pod := &corev1.Pod{}
	pod.Name = sandbox.Name
	pod.Namespace = ns.Name
	tc.MustExist(pod)
	initialCPU := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	initialMem := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]

	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	})

	require.Eventually(t, func() bool {
		current := &sandboxv1alpha1.Sandbox{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, current); err != nil {
			return false
		}
		for _, cond := range current.Status.Conditions {
			if cond.Type == "Ready" && cond.ObservedGeneration == current.Generation {
				return true
			}
		}
		return false
	}, 30*time.Second, time.Second)

	freshPod := &corev1.Pod{}
	freshPod.Name = sandbox.Name
	freshPod.Namespace = ns.Name
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}, freshPod))
	require.Equal(t, initialCPU, freshPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU],
		"pod CPU request should not have changed with None strategy")
	require.Equal(t, initialMem, freshPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory],
		"pod memory request should not have changed with None strategy")
}

func TestUpdateStrategyResize(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-resize-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resize-sandbox",
			Namespace: ns.Name,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			UpdateStrategy: &sandboxv1alpha1.SandboxUpdateStrategy{
				Type: sandboxv1alpha1.ResizeSandboxUpdateStrategyType,
			},
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

	nameHash := NameHash(sandbox.Name)
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "resize-sandbox",
			ServiceFQDN:   fmt.Sprintf("resize-sandbox.%s.svc.cluster.local", ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Message:            "Pod is Ready; Service Exists",
					ObservedGeneration: 1,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	}
	tc.MustWaitForObject(sandbox, p...)

	desiredCPU := resource.MustParse("500m")
	desiredMem := resource.MustParse("256Mi")
	framework.MustUpdateObject(tc.ClusterClient, sandbox, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = desiredCPU
		obj.Spec.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = desiredMem
	})

	require.Eventually(t, func() bool {
		currentPod := &corev1.Pod{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: sandbox.Name, Namespace: ns.Name}, currentPod); err != nil {
			return false
		}
		gotCPU := currentPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
		gotMem := currentPod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
		return gotCPU.Equal(desiredCPU) && gotMem.Equal(desiredMem)
	}, 30*time.Second, time.Second, "pod resources should be resized to match the updated podTemplate")
}
