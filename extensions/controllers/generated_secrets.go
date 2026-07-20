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
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	defaultGeneratedSecretLengthBytes = 32
	maxGeneratedSecretLengthBytes     = 128
	maxKubernetesNameLength           = 253
)

func assignGeneratedSandboxName(sandbox *sandboxv1beta1.Sandbox) error {
	if sandbox.Name != "" {
		return nil
	}

	random := make([]byte, 5)
	if _, err := cryptorand.Read(random); err != nil {
		return fmt.Errorf("generate sandbox name suffix: %w", err)
	}
	suffix := hex.EncodeToString(random)
	prefix := strings.TrimSuffix(sandbox.GenerateName, "-")
	sandbox.Name = nameWithSuffix(prefix, suffix)
	sandbox.GenerateName = ""
	return nil
}

func nameWithSuffix(prefix, suffix string) string {
	name := prefix + "-" + suffix
	if len(name) <= maxKubernetesNameLength {
		return name
	}

	digest := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(digest[:])[:10]
	maxPrefixLength := maxKubernetesNameLength - len(hash) - 1
	prefix = strings.TrimRight(prefix[:maxPrefixLength], ".-")
	return prefix + "-" + hash
}

func prepareGeneratedSecretReferences(sandbox *sandboxv1beta1.Sandbox, specs []extensionsv1beta1.GeneratedSecretSpec) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if sandbox.Name == "" {
		return nil, errors.New("sandbox name must be assigned before resolving generated Secrets")
	}

	declared := make(map[string]map[string]struct{}, len(specs))
	for _, spec := range specs {
		if problems := validation.IsDNS1123Label(spec.Name); len(problems) > 0 {
			return nil, fmt.Errorf("generated Secret logical name %q is invalid: %s", spec.Name, strings.Join(problems, "; "))
		}
		if _, exists := declared[spec.Name]; exists {
			return nil, fmt.Errorf("generated Secret logical name %q is duplicated", spec.Name)
		}
		if len(spec.Data) == 0 {
			return nil, fmt.Errorf("generated Secret %q must declare at least one data key", spec.Name)
		}

		keys := make(map[string]struct{}, len(spec.Data))
		for _, entry := range spec.Data {
			if problems := validation.IsConfigMapKey(entry.Key); len(problems) > 0 {
				return nil, fmt.Errorf("generated Secret %q data key %q is invalid: %s", spec.Name, entry.Key, strings.Join(problems, "; "))
			}
			if _, exists := keys[entry.Key]; exists {
				return nil, fmt.Errorf("generated Secret %q data key %q is duplicated", spec.Name, entry.Key)
			}
			if entry.LengthBytes != 0 && (entry.LengthBytes < defaultGeneratedSecretLengthBytes || entry.LengthBytes > maxGeneratedSecretLengthBytes) {
				return nil, fmt.Errorf("generated Secret %q data key %q lengthBytes must be between %d and %d", spec.Name, entry.Key, defaultGeneratedSecretLengthBytes, maxGeneratedSecretLengthBytes)
			}
			keys[entry.Key] = struct{}{}
		}
		declared[spec.Name] = keys
	}

	used := make(map[string]bool, len(specs))
	visit := func(env []corev1.EnvVar) error {
		for i := range env {
			selector := env[i].ValueFrom
			if selector == nil || selector.SecretKeyRef == nil {
				continue
			}
			logicalName := selector.SecretKeyRef.Name
			keys, symbolic := declared[logicalName]
			if !symbolic {
				continue
			}
			if _, exists := keys[selector.SecretKeyRef.Key]; !exists {
				return fmt.Errorf("secretKeyRef %q references undeclared key %q", logicalName, selector.SecretKeyRef.Key)
			}
			used[logicalName] = true
		}
		return nil
	}
	for i := range sandbox.Spec.PodTemplate.Spec.InitContainers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.InitContainers[i].Env); err != nil {
			return nil, err
		}
	}
	for i := range sandbox.Spec.PodTemplate.Spec.Containers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.Containers[i].Env); err != nil {
			return nil, err
		}
	}
	for i := range sandbox.Spec.PodTemplate.Spec.EphemeralContainers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.EphemeralContainers[i].Env); err != nil {
			return nil, err
		}
	}
	for logicalName := range declared {
		if !used[logicalName] {
			return nil, fmt.Errorf("generated Secret %q is not referenced by the Pod template", logicalName)
		}
	}

	resolved := make(map[string]string, len(specs))
	for _, spec := range specs {
		resolved[spec.Name] = nameWithSuffix(sandbox.Name, spec.Name)
	}
	rewrite := func(env []corev1.EnvVar) {
		for i := range env {
			selector := env[i].ValueFrom
			if selector == nil || selector.SecretKeyRef == nil {
				continue
			}
			if concreteName, symbolic := resolved[selector.SecretKeyRef.Name]; symbolic {
				selector.SecretKeyRef.Name = concreteName
			}
		}
	}
	for i := range sandbox.Spec.PodTemplate.Spec.InitContainers {
		rewrite(sandbox.Spec.PodTemplate.Spec.InitContainers[i].Env)
	}
	for i := range sandbox.Spec.PodTemplate.Spec.Containers {
		rewrite(sandbox.Spec.PodTemplate.Spec.Containers[i].Env)
	}
	for i := range sandbox.Spec.PodTemplate.Spec.EphemeralContainers {
		rewrite(sandbox.Spec.PodTemplate.Spec.EphemeralContainers[i].Env)
	}

	serialized, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("serialize generated Secret names: %w", err)
	}
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}
	sandbox.Annotations[extensionsv1beta1.GeneratedSecretsAnnotation] = string(serialized)
	return resolved, nil
}

func provisionGeneratedSecrets(
	ctx context.Context,
	kubeClient client.Client,
	scheme *runtime.Scheme,
	sandbox *sandboxv1beta1.Sandbox,
	specs []extensionsv1beta1.GeneratedSecretSpec,
	resolved map[string]string,
) error {
	created, err := ensureGeneratedSecrets(ctx, kubeClient, scheme, sandbox, specs, resolved)
	if err != nil {
		cleanupErr := cleanupGeneratedSecrets(ctx, kubeClient, created)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func ensureGeneratedSecrets(
	ctx context.Context,
	kubeClient client.Client,
	scheme *runtime.Scheme,
	sandbox *sandboxv1beta1.Sandbox,
	specs []extensionsv1beta1.GeneratedSecretSpec,
	resolved map[string]string,
) ([]*corev1.Secret, error) {
	if err := validateResolvedGeneratedSecretReferences(sandbox, specs, resolved); err != nil {
		return nil, err
	}

	created := make([]*corev1.Secret, 0, len(specs))
	for _, spec := range specs {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: sandbox.Namespace, Name: resolved[spec.Name]}
		if err := kubeClient.Get(ctx, key, secret); err == nil {
			if err := validateGeneratedSecret(secret, sandbox, spec); err != nil {
				return created, err
			}
			continue
		} else if !k8serrors.IsNotFound(err) {
			return created, fmt.Errorf("get generated Secret %q: %w", key.Name, err)
		}

		secret, err := newGeneratedSecret(sandbox, spec, resolved[spec.Name], scheme)
		if err != nil {
			return created, err
		}
		if err := kubeClient.Create(ctx, secret); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return created, fmt.Errorf("create generated Secret %q: %w", secret.Name, err)
			}
			concurrent := &corev1.Secret{}
			if getErr := kubeClient.Get(ctx, key, concurrent); getErr != nil {
				return created, fmt.Errorf("get concurrently created generated Secret %q: %w", key.Name, getErr)
			}
			if validationErr := validateGeneratedSecret(concurrent, sandbox, spec); validationErr != nil {
				return created, validationErr
			}
			continue
		}
		created = append(created, secret)
	}
	return created, nil
}

func resolvedGeneratedSecretNames(sandbox *sandboxv1beta1.Sandbox, specs []extensionsv1beta1.GeneratedSecretSpec) (map[string]string, error) {
	if len(specs) == 0 {
		if sandbox.Annotations[extensionsv1beta1.GeneratedSecretsAnnotation] != "" {
			return nil, errors.New("generated Secrets annotation exists without declarations")
		}
		return nil, nil
	}
	serialized := sandbox.Annotations[extensionsv1beta1.GeneratedSecretsAnnotation]
	if serialized == "" {
		return nil, errors.New("generated Secrets annotation is missing")
	}
	resolved := make(map[string]string, len(specs))
	if err := json.Unmarshal([]byte(serialized), &resolved); err != nil {
		return nil, fmt.Errorf("parse generated Secrets annotation: %w", err)
	}
	if err := validateResolvedGeneratedSecretReferences(sandbox, specs, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateResolvedGeneratedSecretReferences(
	sandbox *sandboxv1beta1.Sandbox,
	specs []extensionsv1beta1.GeneratedSecretSpec,
	resolved map[string]string,
) error {
	if len(specs) == 0 {
		if len(resolved) != 0 {
			return errors.New("generated Secret names exist without declarations")
		}
		return nil
	}
	if len(resolved) != len(specs) {
		return fmt.Errorf("generated Secrets annotation has %d entries, expected %d", len(resolved), len(specs))
	}

	declaredKeys := make(map[string]map[string]struct{}, len(specs))
	actualToLogical := make(map[string]string, len(specs))
	for _, spec := range specs {
		actual, exists := resolved[spec.Name]
		if !exists {
			return fmt.Errorf("generated Secrets annotation is missing logical name %q", spec.Name)
		}
		expected := nameWithSuffix(sandbox.Name, spec.Name)
		if actual != expected {
			return fmt.Errorf("generated Secret %q resolves to %q, expected %q", spec.Name, actual, expected)
		}
		keys := make(map[string]struct{}, len(spec.Data))
		for _, entry := range spec.Data {
			keys[entry.Key] = struct{}{}
		}
		declaredKeys[spec.Name] = keys
		actualToLogical[actual] = spec.Name
	}

	used := make(map[string]bool, len(specs))
	visit := func(env []corev1.EnvVar) error {
		for i := range env {
			selector := env[i].ValueFrom
			if selector == nil || selector.SecretKeyRef == nil {
				continue
			}
			logical, generated := actualToLogical[selector.SecretKeyRef.Name]
			if !generated {
				if _, unresolved := resolved[selector.SecretKeyRef.Name]; unresolved {
					return fmt.Errorf("secretKeyRef %q was not rewritten to its concrete generated Secret name", selector.SecretKeyRef.Name)
				}
				continue
			}
			if _, exists := declaredKeys[logical][selector.SecretKeyRef.Key]; !exists {
				return fmt.Errorf("generated secretKeyRef %q references undeclared key %q", selector.SecretKeyRef.Name, selector.SecretKeyRef.Key)
			}
			used[logical] = true
		}
		return nil
	}
	for i := range sandbox.Spec.PodTemplate.Spec.InitContainers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.InitContainers[i].Env); err != nil {
			return err
		}
	}
	for i := range sandbox.Spec.PodTemplate.Spec.Containers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.Containers[i].Env); err != nil {
			return err
		}
	}
	for i := range sandbox.Spec.PodTemplate.Spec.EphemeralContainers {
		if err := visit(sandbox.Spec.PodTemplate.Spec.EphemeralContainers[i].Env); err != nil {
			return err
		}
	}
	for _, spec := range specs {
		if !used[spec.Name] {
			return fmt.Errorf("generated Secret %q is not referenced by its concrete name", spec.Name)
		}
	}
	return nil
}

func newGeneratedSecret(
	sandbox *sandboxv1beta1.Sandbox,
	spec extensionsv1beta1.GeneratedSecretSpec,
	name string,
	scheme *runtime.Scheme,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sandbox.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       make(map[string][]byte, len(spec.Data)),
	}
	for _, entry := range spec.Data {
		length := entry.LengthBytes
		if length == 0 {
			length = defaultGeneratedSecretLengthBytes
		}
		random := make([]byte, length)
		if _, err := cryptorand.Read(random); err != nil {
			return nil, fmt.Errorf("generate data for Secret %q key %q: %w", secret.Name, entry.Key, err)
		}
		secret.Data[entry.Key] = []byte(base64.RawURLEncoding.EncodeToString(random))
	}
	if err := controllerutil.SetControllerReference(sandbox, secret, scheme); err != nil {
		return nil, fmt.Errorf("set Sandbox owner on generated Secret %q: %w", secret.Name, err)
	}
	return secret, nil
}

func validateGeneratedSecret(secret *corev1.Secret, sandbox *sandboxv1beta1.Sandbox, spec extensionsv1beta1.GeneratedSecretSpec) error {
	if !metav1.IsControlledBy(secret, sandbox) {
		return fmt.Errorf("generated Secret %q is not controlled by Sandbox %q", secret.Name, sandbox.Name)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return fmt.Errorf("generated Secret %q has type %q, expected %q", secret.Name, secret.Type, corev1.SecretTypeOpaque)
	}
	for _, entry := range spec.Data {
		value, exists := secret.Data[entry.Key]
		if !exists || len(value) == 0 {
			return fmt.Errorf("generated Secret %q is missing non-empty data key %q", secret.Name, entry.Key)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(string(value))
		if err != nil {
			return fmt.Errorf("generated Secret %q data key %q is not a URL-safe token: %w", secret.Name, entry.Key, err)
		}
		expectedLength := entry.LengthBytes
		if expectedLength == 0 {
			expectedLength = defaultGeneratedSecretLengthBytes
		}
		if len(decoded) != int(expectedLength) {
			return fmt.Errorf("generated Secret %q data key %q decodes to %d bytes, expected %d", secret.Name, entry.Key, len(decoded), expectedLength)
		}
	}
	return nil
}

func cleanupGeneratedSecrets(ctx context.Context, kubeClient client.Client, secrets []*corev1.Secret) error {
	var cleanupErr error
	for _, secret := range secrets {
		if err := client.IgnoreNotFound(kubeClient.Delete(ctx, secret)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete generated Secret %q: %w", secret.Name, err))
		}
	}
	return cleanupErr
}
