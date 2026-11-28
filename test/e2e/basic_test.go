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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func simpleSandbox(ns string) *sandboxv1beta1.Sandbox {
	sandboxObj := &sandboxv1beta1.Sandbox{}
	sandboxObj.Name = "my-sandbox"
	sandboxObj.Namespace = ns
	sandboxObj.Spec.Service = new(true)
	sandboxObj.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{ // Use a simple pause container as a basic test
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				},
			},
		},
		ObjectMeta: sandboxv1beta1.PodMetadata{
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

	// Field-level assertions on the Sandbox status, Pod and Service live in the
	// controller unit tests. This test only checks that a real cluster runs them.
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, predicates.ReadyConditionIsTrue))

	pod := &corev1.Pod{}
	pod.Name = sandboxObj.Name
	pod.Namespace = ns.Name
	// A real API server must keep the template metadata instead of pruning it.
	tc.MustMatchPredicates(pod,
		predicates.HasAnnotation("test-anno-key", "val-1"),
		predicates.HasLabel("test-label-key", "val-2"),
	)

	service := &corev1.Service{}
	service.Name = sandboxObj.Name
	service.Namespace = ns.Name
	tc.MustExist(service)
}

func TestSandboxServiceNameValidation(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sandbox-name-validation-%d", time.Now().UnixNano())}}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	newSandbox := func(name string, service *bool) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
			Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: service,
				PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				}}}},
			}},
		}
	}

	t.Run("63 characters with service succeeds", func(t *testing.T) {
		err := tc.CreateWithCleanup(t.Context(), newSandbox(strings.Repeat("a", 63), new(true)))
		require.NoError(t, err)
	})

	t.Run("64 characters with service is rejected", func(t *testing.T) {
		err := tc.CreateWithCleanup(t.Context(), newSandbox(strings.Repeat("b", 64), new(true)))
		require.ErrorContains(t, err, "metadata.name must not exceed 63 characters when spec.service is true")
	})

	t.Run("64 characters without service succeeds but enabling service is rejected", func(t *testing.T) {
		sandbox := newSandbox(strings.Repeat("c", 64), new(false))
		require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))

		sandbox.Spec.Service = new(true)
		err := tc.Update(t.Context(), sandbox)
		require.ErrorContains(t, err, "metadata.name must not exceed 63 characters when spec.service is true")
	})

	t.Run("64 characters with service unset succeeds", func(t *testing.T) {
		err := tc.CreateWithCleanup(t.Context(), newSandbox(strings.Repeat("d", 64), nil))
		require.NoError(t, err)
	})
}
