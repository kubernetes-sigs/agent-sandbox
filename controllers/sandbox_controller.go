/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

const (
	sandboxFinalizer = "agents.x-k8s.io/finalizer"
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Sandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	sandbox := &sandboxv1alpha1.Sandbox{}
	if err := r.Get(ctx, req.NamespacedName, sandbox); err != nil {
		if errors.IsNotFound(err) {
			log.Info("sandbox resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get sandbox")
		return ctrl.Result{}, err
	}

	// Check if the sandbox is being deleted
	if !sandbox.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(sandbox, sandboxFinalizer) {
			if err := r.finalizeSandbox(ctx, sandbox); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(sandbox, sandboxFinalizer)
			if err := r.Update(ctx, sandbox); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(sandbox, sandboxFinalizer) {
		controllerutil.AddFinalizer(sandbox, sandboxFinalizer)
		if err := r.Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if the pod already exists, if not create a new one
	found := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		// Define a new pod
		pod, err := r.podForSandbox(sandbox)
		if err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Creating a new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		if err = r.Create(ctx, pod); err != nil {
			log.Error(err, "Failed to create new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Pod")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) finalizeSandbox(ctx context.Context, s *sandboxv1alpha1.Sandbox) error {
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: s.Namespace}, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get pod during finalization: %w", err)
	}

	if err := r.Delete(ctx, pod); err != nil {
		return fmt.Errorf("failed to delete pod during finalization: %w", err)
	}
	return nil
}

func (r *SandboxReconciler) podForSandbox(s *sandboxv1alpha1.Sandbox) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
		},
		Spec: s.Spec.Template.Spec,
	}
	if err := ctrl.SetControllerReference(s, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
