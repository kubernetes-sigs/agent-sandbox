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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestSharedAgentSandboxProjections(t *testing.T) {
	var fixtures []struct {
		Name                  string
		Claim                 extensionsv1beta1.SandboxClaim
		Namespace             corev1.Namespace
		Sandbox               sandboxv1beta1.Sandbox
		Pod                   corev1.Pod
		ClaimIntentProjection ClaimIntentProjection
		ClaimIntentDigest     string
		MetadataProjection    MetadataProjection
		MetadataDigest        string
	}
	require.NoError(t, json.Unmarshal(fixtureBytes(t, "agent-sandbox-projections.json"), &fixtures))
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			intent, err := ClaimIntentDigest(&fixture.Claim)
			require.NoError(t, err)
			require.Equal(t, fixture.ClaimIntentDigest, string(intent))
			projectionDigest, err := Digest(ClaimIntentDomain, fixture.ClaimIntentProjection)
			require.NoError(t, err)
			require.Equal(t, intent, projectionDigest)
			metadata, err := MetadataDigest(&fixture.Namespace, &fixture.Sandbox, &fixture.Pod)
			require.NoError(t, err)
			require.Equal(t, fixture.MetadataDigest, string(metadata))
			projectionDigest, err = Digest(MetadataDomain, fixture.MetadataProjection)
			require.NoError(t, err)
			require.Equal(t, metadata, projectionDigest)

			// Publishing assignment and metrics cannot invalidate the intent.
			changed := fixture.Claim.DeepCopy()
			if changed.Annotations == nil {
				changed.Annotations = map[string]string{}
			}
			changed.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] = "controller-output"
			changed.Annotations["agents.x-k8s.io/create-time"] = "controller-output"
			actual, err := ClaimIntentDigest(changed)
			require.NoError(t, err)
			require.Equal(t, intent, actual)
			changed.Annotations["opentelemetry.io/trace-context"] = "changed-target-input"
			actual, err = ClaimIntentDigest(changed)
			require.NoError(t, err)
			require.NotEqual(t, intent, actual)
		})
	}
}
