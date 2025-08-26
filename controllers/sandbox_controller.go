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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch

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
		return ctrl.Result{}, err
	}

	if !sandbox.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Sandbox is being deleted")
		return ctrl.Result{}, nil
	}

	// Check if the pod already exists, if not create a new one
	podInCluster := &corev1.Pod{}
	found := true
	err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, podInCluster)
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get Pod")
			return ctrl.Result{}, err
		}
		found = false
	}

	// Create a pod object from the sandbox
	pod, err := r.podForSandbox(sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		log.Info("Creating a new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		if err = r.Create(ctx, pod, client.FieldOwner("sandbox-controller")); err != nil {
			log.Error(err, "Failed to create", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
			return ctrl.Result{}, err
		}
	} else {
		log.Info("Found Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		// TODO - Do we enfore (change) spec if a pod exists ?
		// r.Patch(ctx, pod, client.Apply, client.ForceOwnership, client.FieldOwner("sandbox-controller"))
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) podForSandbox(s *sandboxv1alpha1.Sandbox) (*corev1.Pod, error) {
	// TODO we need to handle this better.
	// We are enforcing the length limitation of label values
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#syntax-and-character-set
	labelValue := s.Name
	if len(labelValue) > 63 {
		labelValue = labelValue[:63]
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels: map[string]string{
				"agents.x-k8s.io/sandbox-name": labelValue,
			},
		},
		Spec: s.Spec.Template.Spec,
	}
	pod.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Pod"))
	if err := ctrl.SetControllerReference(s, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelSelectorPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		// Filter for pods with the agent label
		if pod, ok := object.(*corev1.Pod); ok {
			if _, exists := pod.Labels["agents.x-k8s.io/sandbox-name"]; exists {
				return true
			}
		}
		return false
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}, builder.WithPredicates(labelSelectorPredicate)).
		Complete(r)
}
