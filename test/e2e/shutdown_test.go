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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxShutdownTime(t *testing.T) {
	tc := framework.NewTestContext(t)

	// Set up a namespace
	ns := &corev1.Namespace{}
	ns.Name = "my-sandbox-ns"
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	// Create a Sandbox Object
	sandboxObj := simpleSandbox(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandboxObj))

	nameHash := NameHash(sandboxObj.Name)
	// Assert Sandbox object status reconciles as expected
	p := []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       "my-sandbox",
			ServiceFQDN:   "my-sandbox.my-sandbox-ns.svc.cluster.local",
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
	// Assert Pod and Service objects exist
	pod := &corev1.Pod{}
	pod.Name = "my-sandbox"
	pod.Namespace = "my-sandbox-ns"
	require.NoError(t, tc.ValidateObject(t.Context(), pod))
	service := &corev1.Service{}
	service.Name = "my-sandbox"
	service.Namespace = "my-sandbox-ns"
	require.NoError(t, tc.ValidateObject(t.Context(), service))

	// Set a shutdown time that ends shortly
	shutdown := metav1.NewTime(time.Now().Add(10 * time.Second))
	sandboxObj.Spec.ShutdownTime = &shutdown
	require.NoError(t, tc.Update(t.Context(), sandboxObj))
	// Wait for sandbox status to reflect new state
	p = []predicates.ObjectPredicate{
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			// TODO: should Service/ServiceFQDN be cleared from status when the Service is deleted?
			Service:     "my-sandbox",
			ServiceFQDN: "my-sandbox.my-sandbox-ns.svc.cluster.local",
			Replicas:    0,
			Conditions: []metav1.Condition{
				{
					Message:            "Sandbox has expired",
					ObservedGeneration: 2,
					Reason:             "SandboxExpired",
					Status:             "False",
					Type:               "Ready",
				},
			},
		}),
	}
	require.NoError(t, tc.WaitForObject(t.Context(), sandboxObj, p...))
	// Verify that the sandbox was shut down at or after the specified shutdownTime
	require.True(t, !time.Now().Before(shutdown.Time))
	// Verify Pod and Service are deleted
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), service))
}
