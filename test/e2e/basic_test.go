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

package e2e

import (
	"fmt"
	"hash/fnv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	h := fnv.New32a()
	h.Write([]byte(objectName))
	hashValue := h.Sum32()

	// Convert the uint32 to a hexadecimal string.
	// This results in an 8-character string (e.g., "a5b3c2d1").
	return fmt.Sprintf("%08x", hashValue)
}

func simpleSandbox(ns string) *sandboxv1alpha1.Sandbox {
	sandboxObj := &sandboxv1alpha1.Sandbox{}
	sandboxObj.Name = "my-sandbox"
	sandboxObj.Namespace = ns
	sandboxObj.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{ // Use a simple pause container as a basic test
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
		ObjectMeta: sandboxv1alpha1.PodMetadata{
			Annotations: map[string]string{"test-anno-key": "val-1"},
			Labels:      map[string]string{"test-label-key": "val-2"},
		},
	}
	return sandboxObj
}

func TestSimpleSandbox(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace with unique name to avoid conflicts
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("sandbox-basic-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	// Create a Sandbox Object
	sandboxObj := simpleSandbox(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	// Assert Sandbox object status reconciles as expected
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   fmt.Sprintf("my-sandbox.%s.svc.cluster.local", ns.Name),
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
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, p...))
	// Assert Pod object exists with expected fields
	p = []predicates.ObjectPredicate{
		predicates.HasAnnotation("test-anno-key", "val-1"),
		predicates.HasLabel("test-label-key", "val-2"),
		predicates.HasOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion:         "agents.x-k8s.io/v1alpha1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
				Kind:               "Sandbox",
				Name:               "my-sandbox",
				UID:                sandboxObj.UID,
			},
		}),
	}
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = ns.Name
	require.NoError(t, tc.ValidateObject(t.Context(), pod, p...))
	// Assert Service object exists with expected fields
	p = []predicates.ObjectPredicate{
		predicates.HasOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion:         "agents.x-k8s.io/v1alpha1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
				Kind:               "Sandbox",
				Name:               "my-sandbox",
				UID:                sandboxObj.UID,
			},
		}),
	}
	service := &corev1.Service{}
	service.Name = "my-sandbox"
	service.Namespace = ns.Name
	require.NoError(t, tc.ValidateObject(t.Context(), service, p...))
}
