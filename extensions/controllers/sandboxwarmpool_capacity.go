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
	"errors"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	sandboxSourcePoolUIDIndex      = ".metadata.sourceWarmPoolUID"
	sandboxSourcePoolNameIndex     = ".metadata.sourceWarmPoolName"
	sandboxClaimOwnerUIDIndex      = ".metadata.controllerClaimUID"
	warmPoolNameHashIndex          = ".metadata.membershipNameHash"
	reasonPoolAccountingIncomplete = "WarmPoolAccountingIncomplete"
)

var errPoolAccountingPending = errors.New("pool source update has not been observed")

func warmPoolNameHashIndexer(obj client.Object) []string {
	return []string{sandboxcontrollers.NameHash(obj.GetName())}
}

func warmPoolForClaim(_ context.Context, obj client.Object) []reconcile.Request {
	claim, ok := obj.(*extensionsv1beta1.SandboxClaim)
	if !ok || claim.Spec.WarmPoolRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.WarmPoolRef.Name}}}
}

// Owner UID lets the controller detect missing or conflicting provenance on
// its inventory without a namespace-wide scan. Adoption keeps the source key.
func sandboxSourcePoolUIDIndexer(obj client.Object) []string {
	var keys []string
	if source := obj.GetAnnotations()[sourceWarmPoolUIDAnnotation]; source != "" {
		keys = append(keys, source)
	}
	owner := metav1.GetControllerOf(obj)
	if utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) && owner.UID != "" {
		if len(keys) == 0 || keys[0] != string(owner.UID) {
			keys = append(keys, string(owner.UID))
		}
	}
	return keys
}

func sandboxSourcePoolNameIndexer(obj client.Object) []string {
	if name := obj.GetAnnotations()[sourceWarmPoolNameAnnotation]; name != "" {
		return []string{name}
	}
	return nil
}

func sandboxClaimOwnerUIDIndexer(obj client.Object) []string {
	owner := metav1.GetControllerOf(obj)
	if utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxClaimKind) && owner.UID != "" {
		return []string{string(owner.UID)}
	}
	return nil
}

func sandboxClaimWarmPoolRefIndexer(obj client.Object) []string {
	claim, ok := obj.(*extensionsv1beta1.SandboxClaim)
	if !ok || claim.Spec.WarmPoolRef.Name == "" {
		return nil
	}
	return []string{claim.Spec.WarmPoolRef.Name}
}

func desiredStandbyReplicas(spec *extensionsv1beta1.SandboxWarmPoolSpec, occupied int32) int32 {
	replicas := int32(1)
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}
	if spec.ReplenishmentCapacity == nil {
		return replicas
	}
	remaining := max(int64(0), int64(*spec.ReplenishmentCapacity)-int64(occupied))
	return int32(min(int64(replicas), remaining))
}

// Defaults retain their historical name-based candidate selection. In the
// opt-in mode only the actually resolved pool incarnation can transfer stock.
func verifyClaimAwareCandidate(sb *sandboxv1beta1.Sandbox, pool *extensionsv1beta1.SandboxWarmPool) error {
	if pool.Spec.ReplenishmentCapacity == nil {
		return nil
	}
	owner := metav1.GetControllerOf(sb)
	if !utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) || owner.Name != pool.Name || owner.UID != pool.UID {
		return fmt.Errorf("%w: sandbox %s does not belong to the current pool incarnation", errAdoptionConflict, sb.Name)
	}
	if name, uid := sb.Annotations[sourceWarmPoolNameAnnotation], sb.Annotations[sourceWarmPoolUIDAnnotation]; (name != "" && name != pool.Name) || (uid != "" && uid != string(pool.UID)) {
		return fmt.Errorf("%w: sandbox %s has conflicting pool provenance", errAdoptionConflict, sb.Name)
	}
	return nil
}

func (r *SandboxWarmPoolReconciler) observeClaimAwarePool(ctx context.Context, pool *extensionsv1beta1.SandboxWarmPool) ([]sandboxv1beta1.Sandbox, int32, error) {
	population := &sandboxv1beta1.SandboxList{}
	if err := r.List(ctx, population, client.InNamespace(pool.Namespace), client.MatchingFields{sandboxSourcePoolUIDIndex: string(pool.UID)}); err != nil {
		return nil, 0, fmt.Errorf("observe pool population: %w", err)
	}
	var inventory []sandboxv1beta1.Sandbox
	observed := make(map[string]*sandboxv1beta1.Sandbox, len(population.Items))
	occupied := int64(0)
	for i := range population.Items {
		sb := &population.Items[i]
		observed[sb.Name] = sb
		owner := metav1.GetControllerOf(sb)
		owned := utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) && owner.UID == pool.UID
		if owned && owner.Name != pool.Name {
			return nil, 0, fmt.Errorf("inventory %q has conflicting pool owner name", sb.Name)
		}
		if (owned && sb.Annotations[sourceWarmPoolUIDAnnotation] == "") || (sb.Annotations[sourceWarmPoolUIDAnnotation] == string(pool.UID) && sb.Annotations[sourceWarmPoolNameAnnotation] == "") {
			original := sb.DeepCopy()
			if err := ensureWarmPoolSource(sb, pool.Name, pool.UID); err != nil {
				return nil, 0, err
			}
			if err := r.Patch(ctx, sb, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, 0, fmt.Errorf("record source of inventory %q: %w", sb.Name, err)
			}
			return nil, 0, errPoolAccountingPending
		}
		if sb.Annotations[sourceWarmPoolUIDAnnotation] != string(pool.UID) || sb.Annotations[sourceWarmPoolNameAnnotation] != pool.Name {
			return nil, 0, fmt.Errorf("sandbox %q has incomplete or conflicting source pool identity", sb.Name)
		}
		if owned {
			inventory = append(inventory, *sb)
		} else {
			// A same-source orphan may still contain user state. It is occupied,
			// not an invitation to the legacy inventory adoption/deletion path.
			occupied++
		}
	}
	if err := r.validatePoolCandidates(ctx, pool, observed); err != nil {
		return nil, 0, err
	}
	if occupied > math.MaxInt32 {
		return nil, 0, fmt.Errorf("occupied Sandbox count exceeds status range")
	}
	pool.Status.OccupiedReplicas = new(int32(occupied))
	pool.Status.DesiredStandbyReplicas = new(desiredStandbyReplicas(&pool.Spec, int32(occupied)))
	return inventory, int32(occupied), nil
}

// Auxiliary candidates detect missing provenance; they are never a second
// occupancy tally or permission to adopt an unowned, potentially used instance.
func (r *SandboxWarmPoolReconciler) validatePoolCandidates(ctx context.Context, pool *extensionsv1beta1.SandboxWarmPool, observed map[string]*sandboxv1beta1.Sandbox) error {
	validate := func(sb *sandboxv1beta1.Sandbox) error {
		source := sb.Annotations[sourceWarmPoolUIDAnnotation]
		owner := metav1.GetControllerOf(sb)
		if source == "" {
			if utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) && owner.UID != "" && owner.UID != pool.UID {
				return nil // provably another incarnation's inventory
			}
			return fmt.Errorf("sandbox %q is linked to pool name %q without a provable source UID", sb.Name, pool.Name)
		}
		if source != string(pool.UID) {
			return nil // same name is not proof of the same incarnation
		}
		previous := observed[sb.Name]
		if previous == nil || previous.UID != sb.UID || previous.ResourceVersion != sb.ResourceVersion {
			return errPoolAccountingPending // observations crossed a lifecycle update
		}
		return nil
	}
	for index, value := range map[string]string{
		sandboxSourcePoolNameIndex: pool.Name,
		sandboxWarmPoolLabelIndex:  sandboxcontrollers.NameHash(pool.Name),
	} {
		candidates := &sandboxv1beta1.SandboxList{}
		if err := r.List(ctx, candidates, client.InNamespace(pool.Namespace), client.MatchingFields{index: value}); err != nil {
			return fmt.Errorf("observe pool source candidates: %w", err)
		}
		for i := range candidates.Items {
			if err := validate(&candidates.Items[i]); err != nil {
				return err
			}
		}
	}
	claims := &extensionsv1beta1.SandboxClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(pool.Namespace), client.MatchingFields{extensionsv1beta1.WarmPoolRefField: pool.Name}); err != nil {
		return fmt.Errorf("observe pool-linked claims: %w", err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.UID == "" {
			return fmt.Errorf("claim %q is missing its UID", claim.Name)
		}
		owned := &sandboxv1beta1.SandboxList{}
		if err := r.List(ctx, owned, client.InNamespace(pool.Namespace), client.MatchingFields{sandboxClaimOwnerUIDIndex: string(claim.UID)}); err != nil {
			return fmt.Errorf("observe sandboxes owned by claim %q: %w", claim.Name, err)
		}
		for j := range owned.Items {
			sb := &owned.Items[j]
			owner := metav1.GetControllerOf(sb)
			if !utils.MatchesGroupKind(owner, extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxClaimKind) || owner.UID != claim.UID {
				return errPoolAccountingPending
			}
			if owner.Name != claim.Name {
				return fmt.Errorf("sandbox %q has conflicting claim owner name", sb.Name)
			}
			if err := validate(sb); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *SandboxWarmPoolReconciler) setAccountingIncomplete(pool *extensionsv1beta1.SandboxWarmPool, key client.ObjectKey, incomplete bool, message string) {
	r.notProgressingMu.Lock()
	_, wasIncomplete := r.accountingIncomplete[key]
	if wasIncomplete == incomplete {
		r.notProgressingMu.Unlock()
		return
	}
	if incomplete {
		if r.accountingIncomplete == nil {
			r.accountingIncomplete = make(map[client.ObjectKey]struct{})
		}
		r.accountingIncomplete[key] = struct{}{}
	} else {
		delete(r.accountingIncomplete, key)
	}
	r.notProgressingMu.Unlock()
	if r.Recorder == nil {
		return
	}
	if incomplete {
		r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, reasonPoolAccountingIncomplete, "Accounting", "%s", message)
	} else {
		r.Recorder.Eventf(pool, nil, corev1.EventTypeNormal, "WarmPoolAccountingRecovered", "Accounting", "Pool source accounting is available again")
	}
}
