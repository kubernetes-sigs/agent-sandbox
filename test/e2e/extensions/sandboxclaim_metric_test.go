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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func TestSandboxClaimObservabilityAnnotation(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("claim-obs-anno-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a simple SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "obs-anno-template",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
						},
					},
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	startTime := time.Now()

	// Create a SandboxClaim
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "obs-anno-claim",
			Namespace: ns.Name,
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: "obs-anno-template"},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))

	// Wait for the claim to become ready
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)

	// Retrieve the claim to check annotations
	updatedClaim := &extensionsv1alpha1.SandboxClaim{}
	err := tc.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: ns.Name}, updatedClaim)
	require.NoError(t, err)

	// Check for the annotation
	annoValue, found := updatedClaim.Annotations[asmetrics.ObservabilityAnnotation]
	require.True(t, found, "expected annotation %q to be present", asmetrics.ObservabilityAnnotation)

	// Verify it's a valid RFC3339 timestamp
	parsedTime, err := time.Parse(time.RFC3339Nano, annoValue)
	require.NoError(t, err, "annotation value %q is not a valid RFC3339Nano timestamp", annoValue)

	// Verify it has sub-second precision (non-zero nanoseconds)
	require.NotZero(t, parsedTime.Nanosecond(), "expected non-zero fractional seconds in %q", annoValue)

	// Verify it's reasonably recent (after test started)
	require.True(t, parsedTime.After(startTime) || parsedTime.Equal(startTime), "expected observed time %v to be after test start time %v", parsedTime, startTime)
}
