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

// Package pool contains the controller-side logic for managing pool pods
// that host multiple bubblewrap-isolated Sandbox tenants.
//
// In multi-tenant mode (Sandbox.Spec.Image is set) the controller no longer
// creates a Pod per Sandbox. Instead it selects (or creates) a shared pool
// pod whose base rootfs comes from Sandbox.Spec.Image.Reference, then asks
// the pod-side agent to create a bubblewrap tenant inside that pod. Per-
// sandbox persistent state lives on a subdirectory of a shared PVC.
package pool

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// LabelManagedBy marks pool pods (and their PVCs) as managed by the
	// sandbox controller in multi-tenant mode.
	LabelManagedBy = "agents.x-k8s.io/managed-by"
	// LabelManagedByValue is the value of LabelManagedBy on pool pods.
	LabelManagedByValue = "sandbox-controller-pool"

	// LabelPoolImageHash groups pool pods by the hash of their base image
	// reference. The controller selects pods whose hash matches the requested
	// Sandbox.Spec.Image.Reference.
	LabelPoolImageHash = "agents.x-k8s.io/pool-image-hash"

	// LabelPoolPod identifies a per-pool-pod Service's selector target. Set
	// on the Pod (and copied onto the Service's selector) only when the
	// Provisioner is configured to create per-pool-pod Services (i.e.
	// HTTPRoute mode is on).
	LabelPoolPod = "agents.x-k8s.io/pool-pod"

	// AnnotationPoolImageRef carries the full (unhashed) OCI reference on
	// pool pods for debuggability.
	AnnotationPoolImageRef = "agents.x-k8s.io/pool-image-ref"
)

// DefaultCapacity is the number of bubblewrap tenants a pool pod hosts
// when not overridden by configuration.
const DefaultCapacity = 10

// ImageHash returns a short, stable label-safe hash of an OCI image
// reference, suitable for use as the value of LabelPoolImageHash.
//
// We hash because OCI refs contain characters not legal in label values
// (slashes, colons, @sha256:...) and may exceed the 63-byte label limit.
func ImageHash(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	// 16 hex chars = 64 bits of collision resistance. Plenty for label key
	// uniqueness within a cluster.
	return hex.EncodeToString(sum[:8])
}
