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

package extensions

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

// TestWarmPoolAvailableCondition verifies the controller surfaces the Available
// status condition and observedGeneration on a SandboxWarmPool once its
// sandboxes become ready — the signal automation blocks on via
// `kubectl wait --for=condition=Available sandboxwarmpool/<name>`.
func TestWarmPoolAvailableCondition(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("warmpool-available-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	warmPool.Name = "test-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	replicas := int32(1)
	warmPool.Spec.Replicas = &replicas
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	// Once the pool's single sandbox is ready, the controller should report
	// Available=True with the MinimumReplicasAvailable reason and an
	// observedGeneration matching the pool's generation.
	require.Eventually(t, func() bool {
		got := &extensionsv1beta1.SandboxWarmPool{}
		if err := tc.Get(t.Context(), types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}, got); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, extensionsv1beta1.SandboxWarmPoolConditionAvailable)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			return false
		}
		return cond.Reason == extensionsv1beta1.SandboxWarmPoolMinimumReplicasAvailable &&
			got.Status.ObservedGeneration == got.Generation
	}, 90*time.Second, 2*time.Second, "warm pool should report Available=True once its sandbox is ready")
}
