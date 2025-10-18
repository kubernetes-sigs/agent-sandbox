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

package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// SandboxPolicyReconciler reconciles a Sandbox object for policy purposes
type SandboxPolicyReconciler struct {
	client.Client
}

const (
	safeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update

func (r *SandboxPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	sandbox := &sandboxv1alpha1.Sandbox{}
	if err := r.Get(ctx, req.NamespacedName, sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Reconcile the PodDisruptionBudget
	if err := r.reconcilePDB(ctx, sandbox); err != nil {
		log.Error(err, "Failed to reconcile PDB")
		return ctrl.Result{}, err
	}

	// Reconcile the safe-to-evict annotation on the Pod
	if err := r.reconcilePodAnnotation(ctx, sandbox); err != nil {
		log.Error(err, "Failed to reconcile Pod annotation")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcilePDB manages the PDB for a Sandbox based on its annotation.
func (r *SandboxPolicyReconciler) reconcilePDB(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	log := log.FromContext(ctx)
	pdb := &policyv1.PodDisruptionBudget{}
	pdbName := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	nameHash := NameHash(sandbox.Name)

	// Check if the annotation requests a PDB
	pdbRequested := sandbox.Annotations[sandboxv1alpha1.PDBRequiredAnnotation] == "true"

	if !pdbRequested {
		// If PDB is not requested, ensure it is deleted.
		if err := r.Get(ctx, pdbName, pdb); err != nil {
			return client.IgnoreNotFound(err) // PDB doesn't exist, which is correct.
		}
		log.Info("Deleting PDB as policy annotation is not set", "PDB.Name", pdb.Name)
		return r.Delete(ctx, pdb)
	}

	// If PDB is requested, ensure it exists.
	if err := r.Get(ctx, pdbName, pdb); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("PDB Get Failed: %w", err)
		}
		// PDB does not exist, so create it.
		log.Info("Creating a new PodDisruptionBudget", "PDB.Name", sandbox.Name)
		minAvailable := intstr.FromInt(1)
		newPDB := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &minAvailable,
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{sandboxLabel: nameHash}},
			},
		}
		if err := ctrl.SetControllerReference(sandbox, newPDB, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, newPDB)
	}
	return nil // PDB exists, which is correct.
}

// reconcilePodAnnotation manages the safe-to-evict annotation on the Pod.
func (r *SandboxPolicyReconciler) reconcilePodAnnotation(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), pod); err != nil {
		return client.IgnoreNotFound(err) // Pod may not have been created yet.
	}

	pdbRequested := sandbox.Annotations[sandboxv1alpha1.PDBRequiredAnnotation] == "true"
	patch := client.MergeFrom(pod.DeepCopy())

	if pdbRequested {
		if pod.Annotations[safeToEvictAnnotation] == "false" {
			return nil // Annotation is already correct.
		}
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[safeToEvictAnnotation] = "false"
	} else {
		if _, exists := pod.Annotations[safeToEvictAnnotation]; !exists {
			return nil // Annotation is already absent.
		}
		delete(pod.Annotations, safeToEvictAnnotation)
	}

	return r.Patch(ctx, pod, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.Sandbox{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("sandboxpolicy").
		Complete(r)
}
