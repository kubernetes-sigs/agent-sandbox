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
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

func generatedSecretTemplate() (*extensionsv1beta1.SandboxTemplate, []extensionsv1beta1.GeneratedSecretSpec) {
	specs := []extensionsv1beta1.GeneratedSecretSpec{{
		Name: "execd-credential",
		Data: []extensionsv1beta1.GeneratedSecretDataSpec{{Key: "token"}},
	}}
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "execd",
					Image: "execd:test",
					Env: []corev1.EnvVar{
						{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "execd-credential"}, Key: "token"}}},
						{Name: "STATIC", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "existing-secret"}, Key: "value"}}},
					},
				}}},
			}},
			GeneratedSecrets: specs,
		},
	}
	return template, specs
}

func TestPrepareGeneratedSecretReferences(t *testing.T) {
	template, specs := generatedSecretTemplate()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a"},
		Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
	}

	resolved, err := prepareGeneratedSecretReferences(sandbox, specs)
	require.NoError(t, err)
	require.Equal(t, "sandbox-a-execd-credential", resolved["execd-credential"])
	require.Equal(t, "sandbox-a-execd-credential", sandbox.Spec.PodTemplate.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name)
	require.Equal(t, "existing-secret", sandbox.Spec.PodTemplate.Spec.Containers[0].Env[1].ValueFrom.SecretKeyRef.Name)

	var annotation map[string]string
	require.NoError(t, json.Unmarshal([]byte(sandbox.Annotations[extensionsv1beta1.GeneratedSecretsAnnotation]), &annotation))
	require.Equal(t, resolved, annotation)
	require.NotContains(t, sandbox.Annotations[extensionsv1beta1.GeneratedSecretsAnnotation], "token")
}

func TestPrepareGeneratedSecretReferencesRejectsInvalidDeclarations(t *testing.T) {
	template, specs := generatedSecretTemplate()
	tests := map[string][]extensionsv1beta1.GeneratedSecretSpec{
		"duplicate logical name":    {specs[0], specs[0]},
		"undeclared referenced key": {{Name: "execd-credential", Data: []extensionsv1beta1.GeneratedSecretDataSpec{{Key: "other"}}}},
		"unused declaration":        {{Name: "unused", Data: []extensionsv1beta1.GeneratedSecretDataSpec{{Key: "token"}}}},
	}
	for name, testSpecs := range tests {
		t.Run(name, func(t *testing.T) {
			sandbox := &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a"},
				Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
			}
			_, err := prepareGeneratedSecretReferences(sandbox, testSpecs)
			require.Error(t, err)
		})
	}
}

func TestProvisionGeneratedSecretsAreUniqueAndOwnedBySandbox(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, specs := generatedSecretTemplate()

	tokens := make([][]byte, 0, 2)
	for _, name := range []string{"sandbox-a", "sandbox-b"} {
		template, _ := generatedSecretTemplate()
		sandbox := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
			Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
		}
		resolved, err := prepareGeneratedSecretReferences(sandbox, specs)
		require.NoError(t, err)
		require.NoError(t, provisionGeneratedSecrets(ctx, kubeClient, scheme, sandbox, specs, resolved))

		secret := &corev1.Secret{}
		require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: resolved["execd-credential"]}, secret))
		tokens = append(tokens, secret.Data["token"])
		owner := metav1.GetControllerOf(secret)
		require.NotNil(t, owner)
		require.Equal(t, sandbox.UID, owner.UID)
		require.Len(t, secret.Data["token"], 43)
	}
	require.NotEqual(t, tokens[0], tokens[1])
}

func TestEnsureGeneratedSecretsRejectsWrongOwnerWithoutRotation(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template, specs := generatedSecretTemplate()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: "sandbox-uid"},
		Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
	}
	resolved, err := prepareGeneratedSecretReferences(sandbox, specs)
	require.NoError(t, err)
	originalToken := []byte("not-owned-and-must-not-be-replaced")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: resolved["execd-credential"], Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": originalToken},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, err = ensureGeneratedSecrets(ctx, kubeClient, scheme, sandbox, specs, resolved)
	require.ErrorContains(t, err, "not controlled by Sandbox")
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	require.Equal(t, originalToken, secret.Data["token"])
}

func TestCreatePoolSandboxRollsBackWhenGeneratedSecretCreationFails(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	failingClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				return errors.New("injected Secret create failure")
			}
			return baseClient.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return baseClient.Delete(ctx, obj, opts...)
		},
	}).Build()

	template, specs := generatedSecretTemplate()
	warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-", Namespace: "default"},
		Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
	}
	r := &SandboxWarmPoolReconciler{Client: failingClient, Scheme: scheme}

	err := r.createPoolSandbox(ctx, warmPool, sandbox, specs)
	require.ErrorContains(t, err, "injected Secret create failure")
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, baseClient.List(ctx, list, client.InNamespace("default")))
	require.Empty(t, list.Items)
}

func TestCreatePoolSandboxGeneratesUniqueSecrets(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	template, specs := generatedSecretTemplate()
	warmPool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"}}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-", Namespace: "default"},
		Spec:       sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
	}
	r := &SandboxWarmPoolReconciler{Client: kubeClient, Scheme: scheme}

	require.NoError(t, r.createPoolSandbox(ctx, warmPool, sandbox, specs))
	require.NoError(t, r.createPoolSandbox(ctx, warmPool, sandbox, specs))

	sandboxes := &sandboxv1beta1.SandboxList{}
	require.NoError(t, kubeClient.List(ctx, sandboxes, client.InNamespace("default")))
	require.Len(t, sandboxes.Items, 2)
	secrets := &corev1.SecretList{}
	require.NoError(t, kubeClient.List(ctx, secrets, client.InNamespace("default")))
	require.Len(t, secrets.Items, 2)
	require.NotEqual(t, secrets.Items[0].Name, secrets.Items[1].Name)
	require.NotEqual(t, secrets.Items[0].Data["token"], secrets.Items[1].Data["token"])
	for i := range secrets.Items {
		owner := metav1.GetControllerOf(&secrets.Items[i])
		require.NotNil(t, owner)
		require.Equal(t, "Sandbox", owner.Kind)
	}
}

func TestCreateColdSandboxGeneratesSecret(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	template, _ := generatedSecretTemplate()
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "pool"}},
	}
	r := &SandboxClaimReconciler{Client: kubeClient, Scheme: scheme}

	sandbox, err := r.createSandbox(ctx, claim, template)
	require.NoError(t, err)
	require.Equal(t, "claim-execd-credential", sandbox.Spec.PodTemplate.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name)
	secret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "claim-execd-credential"}, secret))
	require.Equal(t, sandbox.UID, metav1.GetControllerOf(secret).UID)
}

func TestColdSandboxReconcileRestoresMissingGeneratedSecretWithoutRotation(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template, specs := generatedSecretTemplate()
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name}},
		Status:     extensionsv1beta1.SandboxClaimStatus{SandboxStatus: extensionsv1beta1.SandboxStatus{Name: "claim"}},
	}
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim",
			Namespace: "default",
			UID:       "sandbox-uid",
			Labels:    map[string]string{sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeCold},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: "SandboxClaim", Name: claim.Name, UID: claim.UID, Controller: new(true),
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: *template.Spec.SandboxBlueprint.DeepCopy()},
	}
	_, err := prepareGeneratedSecretReferences(sandbox, specs)
	require.NoError(t, err)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template, warmPool, claim, sandbox).Build()
	r := &SandboxClaimReconciler{Client: kubeClient, Scheme: scheme, WarmSandboxQueue: queue.NewSimpleSandboxQueue()}

	_, err = r.reconcileActive(ctx, claim)
	require.NoError(t, err)
	secret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "claim-execd-credential"}, secret))
	firstToken := append([]byte(nil), secret.Data["token"]...)
	require.Equal(t, sandbox.UID, metav1.GetControllerOf(secret).UID)

	_, err = r.reconcileActive(ctx, claim)
	require.NoError(t, err)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	require.Equal(t, firstToken, secret.Data["token"], "reconcile must not rotate an existing valid token")
}

func TestWarmPoolReconcileRestoresMissingGeneratedSecret(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	template, specs := generatedSecretTemplate()
	replicas := int32(1)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	podHash, err := computePodTemplateHash(template)
	require.NoError(t, err)
	blueprintHash, err := computeSandboxBlueprintHash(template)
	require.NoError(t, err)
	r := &SandboxWarmPoolReconciler{Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
	sandbox, err := r.buildSandboxCR(warmPool, sandboxcontrollers.NameHash(warmPool.Name), template, podHash, blueprintHash)
	require.NoError(t, err)
	sandbox.Name = "pool-existing"
	sandbox.GenerateName = ""
	sandbox.UID = "sandbox-uid"
	sandbox.CreationTimestamp = metav1.Now()
	_, err = prepareGeneratedSecretReferences(sandbox, specs)
	require.NoError(t, err)
	kubeClient := newFakeClient(scheme, template, sandbox)
	r.Client = kubeClient

	require.NoError(t, r.reconcilePool(ctx, warmPool))
	secret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pool-existing-execd-credential"}, secret))
	require.Equal(t, sandbox.UID, metav1.GetControllerOf(secret).UID)
}
