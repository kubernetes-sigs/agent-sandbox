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
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

const (
	sandboxLabel = "agents.x-k8s.io/sandbox-name-hash"
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

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
		if k8serrors.IsNotFound(err) {
			log.Info("sandbox resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !sandbox.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Sandbox is being deleted")
		return ctrl.Result{}, nil
	}

	oldStatus := sandbox.Status.DeepCopy()

	// Create a hash from the sandbox.Name and use it as label value
	nameHash := NameHash(sandbox.Name)

	var allErrors error

	// Reconcile Pod
	pod, err := r.reconcilePod(ctx, sandbox, nameHash)
	allErrors = errors.Join(err)

	// Reconcile Service
	svc, err := r.reconcileService(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)

	// compute and set overall Ready condition
	readyCondition := r.computeReadyCondition(sandbox, allErrors, svc, pod)
	meta.SetStatusCondition(&sandbox.Status.Conditions, readyCondition)

	result, err := r.ReconcileSandboxTTL(ctx, sandbox)
	allErrors = errors.Join(allErrors, err)

	// Update status
	err = r.updateStatus(ctx, oldStatus, sandbox)
	allErrors = errors.Join(allErrors, err)

	// return errors seen
	return result, allErrors
}

func (r *SandboxReconciler) computeReadyCondition(sandbox *sandboxv1alpha1.Sandbox, err error, svc *corev1.Service, pod *corev1.Pod) metav1.Condition {
	readyCondition := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		ObservedGeneration: sandbox.Generation,
		Message:            "",
		Status:             metav1.ConditionFalse,
		Reason:             "DependenciesNotReady",
	}

	if err != nil {
		readyCondition.Reason = "ReconcilerError"
		readyCondition.Message = "Error seen: " + err.Error()
		return readyCondition
	}

	message := "Pod or Service not ready"
	podReady := false
	if pod != nil {
		message = "Pod exists with phase: " + string(pod.Status.Phase)
		// Check if pod Ready condition is true
		if pod.Status.Phase == corev1.PodRunning {
			message = "Pod is Running but not Ready"
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady {
					if condition.Status == corev1.ConditionTrue {
						message = "Pod is Ready"
						podReady = true
					}
					break
				}
			}
		}
	}

	svcReady := false
	if svc != nil {
		message += "; Service Exists"
		svcReady = true
	}

	readyCondition.Message = message
	if podReady && svcReady {
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = "DependenciesReady"
		if sandbox.Status.FirstReadyTime == nil {
			now := metav1.Now()
			sandbox.Status.FirstReadyTime = &now
		}
	}

	return readyCondition
}

func (r *SandboxReconciler) updateStatus(ctx context.Context, oldStatus *sandboxv1alpha1.SandboxStatus, sandbox *sandboxv1alpha1.Sandbox) error {
	log := log.FromContext(ctx)

	if reflect.DeepEqual(oldStatus, &sandbox.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, sandbox); err != nil {
		log.Error(err, "Failed to update sandbox status")
		return err
	}

	// Surface error
	return nil
}

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	h := fnv.New32a()
	h.Write([]byte(objectName))
	hashValue := h.Sum32()

	// Convert the uint32 to a hexadecimal string.
	// This results in an 8-character string (e.g., "a5b3c2d1").
	return fmt.Sprintf("%08x", hashValue)
}

func (r *SandboxReconciler) reconcileService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Service, error) {
	log := log.FromContext(ctx)
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, service); err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get Service")
			return nil, fmt.Errorf("Service Get Failed: %w", err)
		}
	} else {
		log.Info("Found Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return service, nil
	}

	log.Info("Creating a new Headless Service", "Service.Namespace", sandbox.Namespace, "Service.Name", sandbox.Name)
	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: nameHash,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector: map[string]string{
				sandboxLabel: nameHash,
			},
		},
	}
	service.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(sandbox, service, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference")
		return nil, fmt.Errorf("SetControllerReference for Service failed: %w", err)
	}

	err := r.Create(ctx, service, client.FieldOwner("sandbox-controller"))
	if err != nil {
		log.Error(err, "Failed to create", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return nil, err
	}

	// TODO(barney-s) : hardcoded to svc.cluster.local which is the default. Need a way to change it.
	sandbox.Status.ServiceFQDN = service.Name + "." + service.Namespace + ".svc.cluster.local"
	sandbox.Status.Service = service.Name
	return service, nil
}

func (r *SandboxReconciler) reconcilePod(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Pod, error) {
	log := log.FromContext(ctx)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, pod); err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get Pod")
			return nil, fmt.Errorf("Pod Get Failed: %w", err)
		}
	} else {
		log.Info("Found Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		// TODO - Do we enfore (change) spec if a pod exists ?
		// r.Patch(ctx, pod, client.Apply, client.ForceOwnership, client.FieldOwner("sandbox-controller"))
		return pod, nil
	}

	// Create a pod object from the sandbox
	log.Info("Creating a new Pod", "Pod.Namespace", sandbox.Namespace, "Pod.Name", sandbox.Name)
	pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: nameHash,
			},
		},
		Spec: sandbox.Spec.PodTemplate.Spec,
	}
	pod.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Pod"))
	if err := ctrl.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("SetControllerReference for Pod failed: %w", err)
	}
	err := r.Create(ctx, pod, client.FieldOwner("sandbox-controller"))
	if err != nil {
		log.Error(err, "Failed to create", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		return nil, err
	}
	return pod, nil
}

// ReconcileSandboxTTL will check if a sandbox has expired, and if so, delete it.
// If the sandbox has not expired, it will requeue the request for the remaining time.

func (r *SandboxReconciler) ReconcileSandboxTTL(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	sandbox.Status.ShutdownAt = nil
	if sandbox.Spec.TTL == nil || (sandbox.Spec.TTL.Seconds == 0 && sandbox.Spec.TTL.ShutdownAt == "") {
		log.Info("Sandbox TTL is not set, skipping TTL check.")
		return reconcile.Result{}, nil
	}

	var expiryTime time.Time
	if sandbox.Spec.TTL.ShutdownAt != "" {
		// Try parsing the endtime as a RFC 3339 string
		fromTime, err := time.Parse(time.RFC3339, sandbox.Spec.TTL.ShutdownAt)
		if err != nil {
			log.Error(err, "Failed to parse TTLFromTime as RFC3339 string", "ShutdownAt", sandbox.Spec.TTL.ShutdownAt)
			return reconcile.Result{}, err
		}
		expiryTime = fromTime.Add(time.Duration(sandbox.Spec.TTL.Seconds) * time.Second)
	} else {
		switch sandbox.Spec.TTL.StartPolicy {
		case sandboxv1alpha1.TTLPolicyOnCreate:
			// Look at the sandbox create time and calculate the endtime
			expiryTime = sandbox.CreationTimestamp.Add(time.Duration(sandbox.Spec.TTL.Seconds) * time.Second)
		case sandboxv1alpha1.TTLPolicyOnReady:
			if sandbox.Status.FirstReadyTime != nil {
				expiryTime = sandbox.Status.FirstReadyTime.Add(time.Duration(sandbox.Spec.TTL.Seconds) * time.Second)
			} else {
				log.Info("Sandbox not yet ready, cannot calculate TTLFromReady")
				return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
			}
		case sandboxv1alpha1.TTLPolicyNever:
			log.Info("TTL is disabled for this sandbox")
			return reconcile.Result{}, nil
		case sandboxv1alpha1.TTLPolicyOnEnable:
			if sandbox.Status.ShutdownAt == nil {
				expiryTime = time.Now().Add(time.Duration(sandbox.Spec.TTL.Seconds) * time.Second)
			}
		default:
			// should not happen
			log.Info("TTL policy unknown: %s", sandbox.Spec.TTL.StartPolicy)
			return reconcile.Result{}, nil
		}
	}

	// Set the .status.ttlExpiryTime
	sandbox.Status.ShutdownAt = &metav1.Time{Time: expiryTime}

	// Calculate remaining time
	remainingTime := time.Until(expiryTime)
	if remainingTime <= 0 {
		log.Info("Sandbox has expired, deleting")
		if err := r.Delete(ctx, sandbox); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete sandbox: %w", err)
		}
		return ctrl.Result{}, nil
	}

	requeueAfter := max(remainingTime/2, 2*time.Second) // Requeue at most every 2 seconds
	log.Info("Requeuing sandbox for TTL", "remaining time", remainingTime, "requeue after", requeueAfter,
		"expiry time", expiryTime)
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	labelSelectorPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      sandboxLabel,
				Operator: metav1.LabelSelectorOpExists,
				Values:   []string{},
			},
		},
	})
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.Sandbox{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(labelSelectorPredicate)).
		Watches(&corev1.Service{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(labelSelectorPredicate)).
		Complete(r)
}
