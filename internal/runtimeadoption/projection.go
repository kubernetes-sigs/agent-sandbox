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

// Package runtimeadoption implements the protected Agent Sandbox side of the
// Gatekeeper Runtime same-policy first-claim contract. It has no runtime module
// dependency; the wire version and canonical fixtures pin the shared encoding.
package runtimeadoption

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const (
	ClaimIntentDomain = "runtime.gatekeeper.sh/agent-sandbox-claim-intent/v1"
	MetadataDomain    = "runtime.gatekeeper.sh/agent-sandbox-metadata/v1"
	EnvelopeDomain    = "runtime.gatekeeper.sh/signed-envelope/v1"
	ContextDomain     = "runtime.gatekeeper.sh/agent-sandbox-adoption-context/v1"
	CommitDomain      = "runtime.gatekeeper.sh/agent-sandbox-adoption-commit-result/v1"
)

// ClaimIntentProjection covers the spec and top-level values propagated to the
// Sandbox. Controller output annotations (assignment, metrics) are deliberately
// absent so publishing an outcome cannot change the accepted intent.
type ClaimIntentProjection struct {
	ClaimUID        string         `json:"claimUID"`
	ClaimGeneration int64          `json:"claimGeneration"`
	Spec            map[string]any `json:"spec"`
	CreatedBy       string         `json:"createdBy"`
	TraceContext    string         `json:"traceContext"`
}

type NamespaceMetadata struct {
	Name   string            `json:"name"`
	UID    string            `json:"uid"`
	Labels map[string]string `json:"labels"`
}

type ObjectMetadata struct {
	Name            string                  `json:"name"`
	UID             string                  `json:"uid"`
	Labels          map[string]string       `json:"labels"`
	Annotations     map[string]string       `json:"annotations"`
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
}

type MetadataProjection struct {
	Namespace NamespaceMetadata `json:"namespace"`
	Sandbox   ObjectMetadata    `json:"sandbox"`
	Pod       ObjectMetadata    `json:"pod"`
}

func ClaimIntentDigest(claim *extensionsv1beta1.SandboxClaim) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	data, err := json.Marshal(claim.Spec)
	if err != nil {
		return "", fmt.Errorf("encode claim intent: %w", err)
	}
	var spec map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&spec); err != nil {
		return "", fmt.Errorf("normalize claim intent: %w", err)
	}
	return Digest(ClaimIntentDomain, ClaimIntentProjection{
		ClaimUID: string(claim.UID), ClaimGeneration: claim.Generation, Spec: spec,
		CreatedBy:    claim.Labels[sandboxv1beta1.CreatedByLabel],
		TraceContext: claim.Annotations["opentelemetry.io/trace-context"],
	})
}

func MetadataDigest(namespace *corev1.Namespace, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	return Digest(MetadataDomain, MetadataProjection{
		Namespace: NamespaceMetadata{Name: namespace.Name, UID: string(namespace.UID), Labels: namespace.Labels},
		Sandbox:   objectMetadata(sandbox.ObjectMeta),
		Pod:       objectMetadata(pod.ObjectMeta),
	})
}

func objectMetadata(metadata metav1.ObjectMeta) ObjectMetadata {
	return ObjectMetadata{Name: metadata.Name, UID: string(metadata.UID), Labels: metadata.Labels,
		Annotations: metadata.Annotations, OwnerReferences: metadata.OwnerReferences}
}

func Digest(domain string, value any) (sandboxv1beta1.RuntimeAdoptionDigest, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytes(domain, data), nil
}

func digestBytes(domain string, data []byte) sandboxv1beta1.RuntimeAdoptionDigest {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(data)
	return sandboxv1beta1.RuntimeAdoptionDigest("sha256:" + hex.EncodeToString(hash.Sum(nil)))
}

// NewID gives each attempt and successor independent, unpredictable identities.
func NewID() (sandboxv1beta1.RuntimeAdoptionID, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate adoption identity: %w", err)
	}
	return sandboxv1beta1.RuntimeAdoptionID(hex.EncodeToString(value[:])), nil
}
