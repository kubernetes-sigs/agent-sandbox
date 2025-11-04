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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

const (
	sandboxLabel                = "agents.x-k8s.io/sandbox-name-hash"
	SanboxPodNameAnnotation     = "agents.x-k8s.io/pod-name"
	sandboxControllerFieldOwner = "sandbox-controller"
)

var (
	// Scheme for use by sandbox controllers. Registers required types for client.
	Scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(Scheme))
}

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

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

	if sandbox.Spec.Replicas == nil {
		replicas := int32(1)
		sandbox.Spec.Replicas = &replicas
	}

	if !sandbox.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Sandbox is being deleted")
		return ctrl.Result{}, nil
	}

	oldStatus := sandbox.Status.DeepCopy()
	var err error

	expired, requeueAfter := checkSandboxExpiry(sandbox)

	// Check if sandbox has expired
	if expired {
		log.Info("Sandbox has expired, deleting pod and service")
		err = r.handleSandboxExpiry(ctx, sandbox)
	} else {
		err = r.reconcileChildResources(ctx, sandbox)
	}

	// Update status
	if statusUpdateErr := r.updateStatus(ctx, oldStatus, sandbox); statusUpdateErr != nil {
		// Surface update error
		err = errors.Join(err, statusUpdateErr)
	}

	// return errors seen
	return ctrl.Result{RequeueAfter: requeueAfter}, err
}

func (r *SandboxReconciler) reconcileChildResources(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	// Create a hash from the sandbox.Name and use it as label value
	nameHash := NameHash(sandbox.Name)

	var allErrors error

	// Reconcile PVCs
	err := r.reconcilePVCs(ctx, sandbox)
	allErrors = errors.Join(allErrors, err)

	// Reconcile Pod
	pod, err := r.reconcilePod(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)
	if pod == nil {
		sandbox.Status.Replicas = 0
		sandbox.Status.LabelSelector = ""
	} else {
		sandbox.Status.Replicas = 1
		sandbox.Status.LabelSelector = fmt.Sprintf("%s=%s", sandboxLabel, NameHash(sandbox.Name))
	}

	// Reconcile Service
	svc, err := r.reconcileService(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)

	// compute and set overall Ready condition
	readyCondition := r.computeReadyCondition(sandbox, allErrors, svc, pod)
	meta.SetStatusCondition(&sandbox.Status.Conditions, readyCondition)

	return allErrors
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

	message := ""
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
	} else {
		if sandbox.Spec.Replicas != nil && *sandbox.Spec.Replicas == 0 {
			message = "Pod does not exist, replicas is 0"
			// This is intended behaviour. So marking it ready.
			podReady = true
		} else {
			message = "Pod does not exist"
		}
	}

	svcReady := false
	if svc != nil {
		message += "; Service Exists"
		svcReady = true
	} else {
		message += "; Service does not exist"
	}

	readyCondition.Message = message
	if podReady && svcReady {
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = "DependenciesReady"
		return readyCondition
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

	err := r.Create(ctx, service, client.FieldOwner(sandboxControllerFieldOwner))
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

	// List all pods with the pool label matching the warm pool name hash
	// TODO: find a better way to make sure one sandbox has at most one pod
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		sandboxLabel: nameHash,
	})

	if err := r.List(ctx, podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     sandbox.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list pods")
	}

	if len(podList.Items) > 1 {
		log.Info("Multiple pods found for sandbox, this should not happen", "Sandbox", sandbox.Name, "PodCount", len(podList.Items))
	}

	// Determine the pod name to look up
	podName := sandbox.Name
	var trackedPodName string
	var podNameAnnotationExists bool
	if trackedPodName, podNameAnnotationExists = sandbox.Annotations[SanboxPodNameAnnotation]; podNameAnnotationExists && trackedPodName != "" {
		podName = trackedPodName
		log.Info("Using tracked pod name from sandbox annotation", "podName", podName)
	}

	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: sandbox.Namespace}, pod)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get Pod")
			return nil, fmt.Errorf("Pod Get Failed: %w", err)
		}
		if k8serrors.IsNotFound(err) && podNameAnnotationExists {
			log.Error(err, "Pod not found")
			return nil, fmt.Errorf("Pod in Annotation Get Failed: %w", err)
		}
		pod = nil
	}

	// if replicas is 0, delete the pod if it exists
	if *sandbox.Spec.Replicas == 0 {
		if pod != nil {
			if pod.ObjectMeta.DeletionTimestamp.IsZero() {
				log.Info("Deleting Pod because .Spec.Replicas is 0", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
				if err := r.Delete(ctx, pod); err != nil {
					return nil, fmt.Errorf("failed to delete pod: %w", err)
				}
			} else {
				log.Info("Pod is already being deleted", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
			}
		}

		// Remove the pod name annotation from the sandbox if it exists
		if _, exists := sandbox.Annotations[SanboxPodNameAnnotation]; exists {
			log.Info("Removing pod name annotation from sandbox", "Sandbox.Name", sandbox.Name)
			// Create a patch to update only the annotations
			patch := client.MergeFrom(sandbox.DeepCopy())
			delete(sandbox.Annotations, SanboxPodNameAnnotation)

			if err := r.Patch(ctx, sandbox, patch); err != nil {
				return nil, fmt.Errorf("failed to remove pod name annotation: %w", err)
			}
		}

		return nil, nil
	}

	if pod != nil {
		log.Info("Found Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)

		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[sandboxLabel] = nameHash

		// Set controller reference if the pod is not controlled by anything.
		if controllerRef := metav1.GetControllerOf(pod); controllerRef == nil {
			if err := ctrl.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
				return nil, fmt.Errorf("SetControllerReference for Pod failed: %w", err)
			}
		}

		if err := r.Update(ctx, pod); err != nil {
			return nil, fmt.Errorf("failed to update pod: %w", err)
		}

		// TODO - Do we enfore (change) spec if a pod exists ?
		// r.Patch(ctx, pod, client.Apply, client.ForceOwnership, client.FieldOwner("sandbox-controller"))
		return pod, nil
	}

	log.Info("Creating a new Pod", "Pod.Namespace", sandbox.Namespace, "Pod.Name", sandbox.Name)
	labels := map[string]string{
		sandboxLabel: nameHash,
	}
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}

	mutatedSpec := sandbox.Spec.PodTemplate.Spec.DeepCopy()

	for _, pvcTemplate := range sandbox.Spec.VolumeClaimTemplates {
		pvcName := pvcTemplate.Name + "-" + sandbox.Name
		mutatedSpec.Volumes = append(mutatedSpec.Volumes, corev1.Volume{
			Name: pvcTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}
	pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandbox.Name,
			Namespace:   sandbox.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *mutatedSpec,
	}
	pod.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Pod"))
	if err := ctrl.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("SetControllerReference for Pod failed: %w", err)
	}
	if err := r.Create(ctx, pod, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		log.Error(err, "Failed to create", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		return nil, err
	}

	return pod, nil
}

func (r *SandboxReconciler) reconcilePVCs(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	log := log.FromContext(ctx)
	for _, pvcTemplate := range sandbox.Spec.VolumeClaimTemplates {
		pvc := &corev1.PersistentVolumeClaim{}
		pvcName := pvcTemplate.Name + "-" + sandbox.Name
		err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: sandbox.Namespace}, pvc)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				log.Info("Creating a new PVC", "PVC.Namespace", sandbox.Namespace, "PVC.Name", pvcName)
				pvc = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: sandbox.Namespace,
					},
					Spec: pvcTemplate.Spec,
				}
				if err := ctrl.SetControllerReference(sandbox, pvc, r.Scheme); err != nil {
					return fmt.Errorf("SetControllerReference for PVC failed: %w", err)
				}
				if err := r.Create(ctx, pvc, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
					log.Error(err, "Failed to create PVC", "PVC.Namespace", sandbox.Namespace, "PVC.Name", pvcName)
					return err
				}
			} else {
				log.Error(err, "Failed to get PVC")
				return fmt.Errorf("PVC Get Failed: %w", err)
			}
		}
	}
	return nil
}

func (r *SandboxReconciler) handleSandboxExpiry(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	var allErrors error
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}
	if err := r.Delete(ctx, pod); err != nil && !k8serrors.IsNotFound(err) {
		allErrors = errors.Join(allErrors, fmt.Errorf("failed to delete pod: %w", err))
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}
	if err := r.Delete(ctx, service); err != nil && !k8serrors.IsNotFound(err) {
		allErrors = errors.Join(allErrors, fmt.Errorf("failed to delete service: %w", err))
	}

	// Update status to remove Ready condition
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             "SandboxExpired",
		Message:            "Sandbox has expired",
	})

	sandbox.Status.Replicas = 0
	sandbox.Status.LabelSelector = ""

	return allErrors
}

// checks if the sandbox has expired
// returns true if expired, false otherwise
// if not expired, also returns the duration to requeue after
func checkSandboxExpiry(sandbox *sandboxv1alpha1.Sandbox) (bool, time.Duration) {
	if sandbox.Spec.ShutdownTime == nil {
		return false, 0
	}

	expiryTime := sandbox.Spec.ShutdownTime.Time
	if time.Now().After(expiryTime) {
		return true, 0
	}

	// Calculate remaining time
	remainingTime := time.Until(expiryTime)

	// TODO(barney-s): Do we need a inverse exponential backoff here ?
	//requeueAfter := max(remainingTime/2, 2*time.Second)

	// Requeue at expiry time or in 2 seconds whichever is later
	requeueAfter := max(remainingTime, 2*time.Second)
	return false, requeueAfter
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
