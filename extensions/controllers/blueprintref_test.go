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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func blueprintRefTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sandboxv1beta1.AddToScheme(scheme))
	utilruntime.Must(extensionsv1beta1.AddToScheme(scheme))
	return scheme
}

func blueprintRefTestTemplate(t *testing.T) (*extensionsv1beta1.SandboxTemplate, string) {
	t.Helper()
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ref-template",
			Namespace: "default",
			UID:       types.UID("ref-template-uid"),
		},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels: map[string]string{"template-pod-label": "yes"},
					},
					Spec: &corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "test-image"}},
					},
				},
			},
		},
	}
	hash, err := computeSandboxBlueprintHash(template)
	require.NoError(t, err)
	return template, hash
}

func Test_buildSandboxCR__BlueprintRefMode(t *testing.T) {
	scheme := blueprintRefTestScheme(t)
	template, hash := blueprintRefTestTemplate(t)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ref-pool",
			Namespace: "default",
			UID:       types.UID("ref-pool-uid"),
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	r := &SandboxWarmPoolReconciler{
		Scheme:                 scheme,
		BlueprintRefEnabled:    true,
		EnableWarmPoolEviction: true,
	}

	poolNameHash := sandboxcontrollers.NameHash(warmPool.Name)
	sandboxCR, err := r.buildSandboxCR(warmPool, poolNameHash, template, "pod-template-hash", hash)
	require.NoError(t, err)

	require.NotNil(t, sandboxCR.Spec.BlueprintRef)
	require.Equal(t, template.Name, sandboxCR.Spec.BlueprintRef.Name)
	require.Equal(t, hash, sandboxCR.Spec.BlueprintRef.BlueprintHash)
	require.Nil(t, sandboxCR.Spec.PodTemplate.Spec, "ref-mode sandbox must not embed the pod spec")
	require.Nil(t, sandboxCR.Spec.VolumeClaimTemplates, "ref-mode sandbox must not embed volume claim templates")
	require.Nil(t, sandboxCR.Spec.Service, "ref-mode sandbox must not embed the service flag")

	// Template pod metadata is carried inline (hydration never fills metadata),
	// and the pool identity labels are layered on top of it.
	require.Equal(t, "yes", sandboxCR.Spec.PodTemplate.ObjectMeta.Labels["template-pod-label"])
	require.Equal(t, poolNameHash, sandboxCR.Spec.PodTemplate.ObjectMeta.Labels[warmPoolSandboxLabel])
	require.Equal(t, hash, sandboxCR.Spec.PodTemplate.ObjectMeta.Labels[sandboxv1beta1.SandboxTemplateHashLabel])
	require.Equal(t, "true", sandboxCR.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation])
	require.Equal(t, hash, sandboxCR.Labels[sandboxv1beta1.SandboxTemplateHashLabel])

	// Sandbox-level labels/annotations are identical to embedded mode; only
	// the blueprint moved behind the reference.
	embeddedR := &SandboxWarmPoolReconciler{Scheme: scheme, EnableWarmPoolEviction: true}
	embeddedCR, err := embeddedR.buildSandboxCR(warmPool, poolNameHash, template, "pod-template-hash", hash)
	require.NoError(t, err)
	require.Equal(t, embeddedCR.Labels, sandboxCR.Labels)
	require.NotNil(t, embeddedCR.Spec.PodTemplate.Spec)
}

func Test_isSandboxStale__BlueprintRefSandbox(t *testing.T) {
	template, hash := blueprintRefTestTemplate(t)
	r := &SandboxWarmPoolReconciler{}

	refSandbox := func(pinnedHash string) *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ref-sandbox",
				Namespace: "default",
				Labels: map[string]string{
					sandboxTemplateRefHash:                  SandboxTemplateRefHash(template.Name),
					sandboxv1beta1.SandboxTemplateHashLabel: pinnedHash,
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{
				BlueprintRef: &sandboxv1beta1.BlueprintRef{Name: template.Name, BlueprintHash: pinnedHash},
			},
		}
		return sb
	}

	vetted := map[string]bool{}
	require.False(t, r.isSandboxStale(t.Context(), refSandbox(hash), template, hash, vetted),
		"pinned hash matching the current template must be fresh")
	require.True(t, r.isSandboxStale(t.Context(), refSandbox("old-hash"), template, hash, vetted),
		"pinned hash differing from the current template must be stale")
	require.False(t, r.isSandboxStale(t.Context(), refSandbox("old-hash"), template, "", vetted),
		"an empty current hash (marshal failure) must never mark ref sandboxes stale")
}

func Test_createSandbox__BlueprintRefMode(t *testing.T) {
	scheme := blueprintRefTestScheme(t)
	template, hash := blueprintRefTestTemplate(t)

	newClaim := func(env []extensionsv1beta1.EnvVar) *extensionsv1beta1.SandboxClaim {
		return &extensionsv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ref-claim",
				Namespace: "default",
				UID:       types.UID("ref-claim-uid"),
			},
			Spec: extensionsv1beta1.SandboxClaimSpec{
				WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "ref-pool"},
				Env:         env,
			},
		}
	}

	t.Run("plain claim references the blueprint", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SandboxClaimReconciler{Client: c, Scheme: scheme, BlueprintRefEnabled: true}

		sandbox, err := r.createSandbox(t.Context(), newClaim(nil), template)
		require.NoError(t, err)
		require.NotNil(t, sandbox.Spec.BlueprintRef)
		require.Equal(t, template.Name, sandbox.Spec.BlueprintRef.Name)
		require.Equal(t, hash, sandbox.Spec.BlueprintRef.BlueprintHash)
		require.Nil(t, sandbox.Spec.PodTemplate.Spec)
		require.Nil(t, sandbox.Spec.VolumeClaimTemplates)
		// Claim identity labels land on the inline pod template metadata, and
		// the template's own pod metadata is preserved beneath them.
		require.Equal(t, "yes", sandbox.Spec.PodTemplate.ObjectMeta.Labels["template-pod-label"])
		require.NotEmpty(t, sandbox.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1beta1.SandboxIDLabel])
	})

	t.Run("claim with env keeps embedding", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SandboxClaimReconciler{Client: c, Scheme: scheme, BlueprintRefEnabled: true}

		envTemplate := template.DeepCopy()
		envTemplate.Spec.EnvVarsInjectionPolicy = extensionsv1beta1.EnvVarsInjectionPolicyAllowed
		sandbox, err := r.createSandbox(t.Context(), newClaim([]extensionsv1beta1.EnvVar{{Name: "FOO", Value: "bar"}}), envTemplate)
		require.NoError(t, err)
		require.Nil(t, sandbox.Spec.BlueprintRef)
		require.NotNil(t, sandbox.Spec.PodTemplate.Spec)
		require.Equal(t, "FOO", sandbox.Spec.PodTemplate.Spec.Containers[0].Env[0].Name)
	})

	t.Run("flag off keeps embedding", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SandboxClaimReconciler{Client: c, Scheme: scheme}

		sandbox, err := r.createSandbox(t.Context(), newClaim(nil), template)
		require.NoError(t, err)
		require.Nil(t, sandbox.Spec.BlueprintRef)
		require.NotNil(t, sandbox.Spec.PodTemplate.Spec)
	})
}
