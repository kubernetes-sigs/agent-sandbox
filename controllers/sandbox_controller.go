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
	"maps"
	"reflect"
	"slices"
	"strings"
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const (
	sandboxLabel                = "agents.x-k8s.io/sandbox-name-hash"
	sandboxControllerFieldOwner = "sandbox-controller"
	immediateRequeueDelay       = time.Millisecond
)

// resourceOwnership represents the ownership state of a Kubernetes resource relative to a Sandbox.
type resourceOwnership int

const (
	// resourceOwnedBySandbox indicates the resource's controllerRef points to this Sandbox.
	resourceOwnedBySandbox resourceOwnership = iota
	// resourceUnowned indicates the resource has no controllerRef.
	resourceUnowned
	// resourceOwnedByOther indicates the resource's controllerRef points to a different controller.
	resourceOwnedByOther
)

// checkOwnership determines whether a Kubernetes resource is owned by the given Sandbox,
// has no controller, or is owned by a different controller.
// It returns both the ownership classification and the controller reference (if any),
// so callers can log owner details without redundant GetControllerOf calls.
func checkOwnership(obj client.Object, sandbox *sandboxv1alpha1.Sandbox) (resourceOwnership, *metav1.OwnerReference) {
	controllerRef := metav1.GetControllerOf(obj)
	if controllerRef == nil {
		return resourceUnowned, nil
	}
	if controllerRef.UID == sandbox.UID {
		return resourceOwnedBySandbox, controllerRef
	}
	return resourceOwnedByOther, controllerRef
}

// resolvePodName returns the name of the pod associated with the given Sandbox.
// If the sandbox has adopted a warm pool pod, the pod name is tracked in the
// agents.x-k8s.io/pod-name annotation and may differ from sandbox.Name.
func resolvePodName(sandbox *sandboxv1alpha1.Sandbox) string {
	if name, ok := sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; ok && name != "" {
		return name
	}
	return sandbox.Name
}

// MergeVolumeClaimVolumes merges PVC-backed volumes into an existing volume
// list, replacing any volumes with matching names. This follows StatefulSet
// semantics where volumeClaimTemplate volumes take priority.
func MergeVolumeClaimVolumes(existing []corev1.Volume, pvcVolumes []corev1.Volume) []corev1.Volume {
	if len(pvcVolumes) == 0 {
		return existing
	}
	vctNames := make(map[string]struct{}, len(pvcVolumes))
	for _, v := range pvcVolumes {
		vctNames[v.Name] = struct{}{}
	}
	filtered := make([]corev1.Volume, 0, len(existing))
	for _, v := range existing {
		if _, ok := vctNames[v.Name]; !ok {
			filtered = append(filtered, v)
		}
	}
	return append(filtered, pvcVolumes...)
}

var (
	// Scheme for use by sandbox controllers. Registers required types for client.
	Scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(Scheme))
}

// SandboxReconciler reconciles a Sandbox object.
type SandboxReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Tracer        asmetrics.Instrumenter
	ClusterDomain string
}

//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/finalizers,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch

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

	// Start Tracing Span
	initialAttrs := map[string]string{
		"sandbox.name":      sandbox.Name,
		"sandbox.namespace": sandbox.Namespace,
	}
	ctx, end := r.Tracer.StartSpan(ctx, sandbox, "ReconcileSandbox", initialAttrs)
	defer end()

	// If the sandbox is being deleted, do nothing
	if !sandbox.DeletionTimestamp.IsZero() {
		log.Info("Sandbox is being deleted")

		oldStatus := sandbox.Status.DeepCopy()
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			ObservedGeneration: sandbox.Generation,
			Status:             metav1.ConditionFalse,
			Reason:             sandboxv1alpha1.SandboxReasonDeleting,
			Message:            "Sandbox is terminating",
		})
		if err := r.updateStatus(ctx, oldStatus, sandbox); err != nil {
			log.Error(err, "Failed to update status for terminating sandbox")
		}

		return ctrl.Result{}, nil
	}

	// Initialize trace ID for active resources missing an ID (inline, no re-reconcile)
	tc := r.Tracer.GetTraceContext(ctx)
	if tc != "" && (sandbox.Annotations == nil || sandbox.Annotations[asmetrics.TraceContextAnnotation] == "") {
		patch := client.MergeFrom(sandbox.DeepCopy())
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations[asmetrics.TraceContextAnnotation] = tc

		if err := r.Patch(ctx, sandbox, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	if sandbox.Spec.Replicas == nil {
		sandbox.Spec.Replicas = ptr.To[int32](1)
	}

	oldStatus := sandbox.Status.DeepCopy()
	var err error
	sandboxDeleted := false
	result := ctrl.Result{}

	expired, _ := checkSandboxExpiry(sandbox, time.Now())
	if expired {
		if !sandboxMarkedExpired(sandbox) {
			setSandboxExpiredCondition(sandbox)
			if statusUpdateErr := r.updateStatus(ctx, oldStatus, sandbox); statusUpdateErr != nil {
				return ctrl.Result{}, statusUpdateErr
			}
			return ctrl.Result{RequeueAfter: immediateRequeueDelay}, nil
		}

		log.Info("Sandbox has expired, deleting child resources and checking shutdown policy")
		sandboxDeleted, err = r.handleSandboxExpiry(ctx, sandbox)
	} else {
		err = r.reconcileChildResources(ctx, sandbox)
		expiredAfterReconcile, requeueAfter := checkSandboxExpiry(sandbox, time.Now())
		result.RequeueAfter = requeueAfter
		if expiredAfterReconcile {
			setSandboxExpiredCondition(sandbox)
			result.RequeueAfter = immediateRequeueDelay
		}
	}

	if !sandboxDeleted {
		// Update status
		if statusUpdateErr := r.updateStatus(ctx, oldStatus, sandbox); statusUpdateErr != nil {
			// Surface update error
			err = errors.Join(err, statusUpdateErr)
		}
	}
	// return errors seen
	return result, err
}

func (r *SandboxReconciler) reconcileChildResources(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	// Create a hash from the sandbox.Name and use it as label value
	nameHash := NameHash(sandbox.Name)

	var allErrors error

	// Reconcile PVCs from volumeClaimTemplates
	err := r.reconcilePVCs(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)
	pvcsProvisioned := err == nil

	// Reconcile Pod
	pod, err := r.reconcilePod(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)
	if pod == nil {
		sandbox.Status.Replicas = 0
		sandbox.Status.LabelSelector = ""
		sandbox.Status.PodIPs = nil
	} else {
		sandbox.Status.Replicas = 1
		sandbox.Status.LabelSelector = fmt.Sprintf("%s=%s", sandboxLabel, NameHash(sandbox.Name))
		sandbox.Status.PodIPs = podIPsFromStatus(pod.Status.PodIPs)
	}

	// Reconcile Service
	_, err = r.reconcileService(ctx, sandbox, nameHash)
	allErrors = errors.Join(allErrors, err)
	svcsProvisioned := err == nil

	// compute and set overall conditions
	conditions := r.computeConditions(sandbox, svcsProvisioned, pod, pvcsProvisioned)
	for _, condition := range conditions {
		meta.SetStatusCondition(&sandbox.Status.Conditions, condition)
	}

	if finishedCondition := r.computeFinishedCondition(sandbox, pod); finishedCondition != nil {
		meta.SetStatusCondition(&sandbox.Status.Conditions, *finishedCondition)
	} else {
		meta.RemoveStatusCondition(&sandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionFinished))
	}

	return allErrors
}

func (r *SandboxReconciler) computeConditions(sandbox *sandboxv1alpha1.Sandbox, svcsProvisioned bool, pod *corev1.Pod, pvcsProvisioned bool) []metav1.Condition {
	var conditions []metav1.Condition
	gen := sandbox.Generation

	// 1. Initialized Condition
	initialized := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionInitialized),
		ObservedGeneration: gen,
		Status:             metav1.ConditionFalse,
		Reason:             sandboxv1alpha1.SandboxReasonInitializing,
		Message:            "Provisioning dependencies",
	}
	if svcsProvisioned && pvcsProvisioned {
		initialized.Status = metav1.ConditionTrue
		initialized.Reason = sandboxv1alpha1.SandboxReasonInitialized
		initialized.Message = "Service and PVCs are provisioned"
	}
	conditions = append(conditions, initialized)

	// 2. Suspended Condition
	suspended := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionSuspended),
		ObservedGeneration: gen,
		Status:             metav1.ConditionUnknown,
		Reason:             sandboxv1alpha1.SandboxReasonPendingEvaluation,
		Message:            "The suspension status has not yet been determined.",
	}
	isSuspended := sandbox.Spec.Replicas != nil && *sandbox.Spec.Replicas == 0
	if isSuspended {
		suspended.Status = metav1.ConditionTrue
		suspended.Reason = sandboxv1alpha1.SandboxReasonSuspendedNotOperational
		suspended.Message = "Sandbox has been suspended and is not operational"
	} else if pod != nil {
		suspended.Status = metav1.ConditionFalse
		suspended.Reason = sandboxv1alpha1.SandboxReasonNotSuspended
		suspended.Message = "Sandbox is operational and not suspended"
	}
	conditions = append(conditions, suspended)

	// 3. Ready Condition
	ready := metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		ObservedGeneration: gen,
		Status:             metav1.ConditionFalse,
		Reason:             sandboxv1alpha1.SandboxReasonInitializing,
		Message:            "Sandbox is initializing",
	}

	if initialized.Status == metav1.ConditionFalse {
		ready.Status = metav1.ConditionFalse
		ready.Reason = sandboxv1alpha1.SandboxReasonInitializing
		ready.Message = "Waiting for Sandbox to be provisioned"
	} else if isSuspended {
		ready.Status = metav1.ConditionFalse
		ready.Reason = sandboxv1alpha1.SandboxReasonSuspended
		ready.Message = "Sandbox is suspended"
	} else if pod == nil {
		ready.Status = metav1.ConditionFalse
		ready.Reason = sandboxv1alpha1.SandboxReasonPodInitializing
		ready.Message = "Pod is initializing"
	} else {
		// Pod exists
		switch pod.Status.Phase {
		case corev1.PodRunning:
			podIsReady := false
			podMessage := "Pod is not Ready"
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady {
					if condition.Status == corev1.ConditionTrue {
						if len(pod.Status.PodIPs) == 0 {
							podMessage = "Pod is Ready but has no podIPs yet"
						} else {
							podIsReady = true
						}
					}
					break
				}
			}
			if podIsReady {
				ready.Status = metav1.ConditionTrue
				ready.Reason = sandboxv1alpha1.SandboxReasonReady
				ready.Message = "Sandbox is operational"
			} else {
				ready.Status = metav1.ConditionFalse
				ready.Reason = sandboxv1alpha1.SandboxReasonPodNotReady
				ready.Message = podMessage
			}
		case corev1.PodUnknown:
			ready.Status = metav1.ConditionUnknown
			ready.Reason = sandboxv1alpha1.SandboxReasonUnresponsive
			ready.Message = "Pod status is unknown"
		case corev1.PodSucceeded:
			ready.Status = metav1.ConditionFalse
			ready.Reason = sandboxv1alpha1.SandboxReasonPodSucceeded
			ready.Message = "Pod completed successfully"
		case corev1.PodFailed:
			ready.Status = metav1.ConditionFalse
			ready.Reason = sandboxv1alpha1.SandboxReasonPodFailed
			ready.Message = "Pod failed"
		default:
			ready.Status = metav1.ConditionFalse
			ready.Reason = sandboxv1alpha1.SandboxReasonPodNotReady
			ready.Message = "Pod is in phase: " + string(pod.Status.Phase)
		}
	}
	conditions = append(conditions, ready)

	return conditions
}

func (r *SandboxReconciler) computeFinishedCondition(sandbox *sandboxv1alpha1.Sandbox, pod *corev1.Pod) *metav1.Condition {
	if pod == nil {
		return nil
	}

	condition := &metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionFinished),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: sandbox.Generation,
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		condition.Reason = sandboxv1alpha1.SandboxReasonPodSucceeded
		condition.Message = "Pod completed successfully"
	case corev1.PodFailed:
		condition.Reason = sandboxv1alpha1.SandboxReasonPodFailed
		condition.Message = "Pod failed"
	default:
		return nil
	}

	return condition
}

// podIPsFromStatus converts the K8s PodIP slice to a plain string slice.
func podIPsFromStatus(podIPs []corev1.PodIP) []string {
	if len(podIPs) == 0 {
		return nil
	}
	ips := make([]string, len(podIPs))
	for i, pip := range podIPs {
		ips[i] = pip.IP
	}
	return ips
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

// GetNumericHash generates a raw FNV-1a hash value.
func GetNumericHash(input string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(input))
	return h.Sum32()
}

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	return fmt.Sprintf("%08x", GetNumericHash(objectName))
}

func (r *SandboxReconciler) reconcileService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Service, error) {
	log := log.FromContext(ctx)
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, service); err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get Service")
			return nil, fmt.Errorf("service get failed: %w", err)
		}
	} else {
		log.Info("Found Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)

		ownership, controllerRef := checkOwnership(service, sandbox)
		switch ownership {
		case resourceOwnedByOther:
			log.Info("Refusing to use service: service is owned by a different controller",
				"Service.Name", service.Name, "Sandbox.Name", sandbox.Name,
				"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)
			return nil, fmt.Errorf("service %q is owned by %s/%s (UID: %s), not by sandbox %q",
				service.Name, controllerRef.Kind, controllerRef.Name, controllerRef.UID, sandbox.Name)

		case resourceUnowned:
			// ClusterIP is immutable — refuse adoption if the service is not headless.
			if service.Spec.ClusterIP != corev1.ClusterIPNone && service.Spec.ClusterIP != "" {
				log.Info("Refusing to adopt service: ClusterIP mismatch (immutable, expected None)",
					"Service.Name", service.Name, "Sandbox.Name", sandbox.Name,
					"Service.ClusterIP", service.Spec.ClusterIP)
				return nil, fmt.Errorf("cannot adopt service %q: ClusterIP is %q (expected %q, field is immutable)",
					service.Name, service.Spec.ClusterIP, corev1.ClusterIPNone)
			}

			log.Info("Adopting unowned service", "Service.Name", service.Name, "Sandbox.Name", sandbox.Name)

			// Enforce intended labels and selector to prevent traffic hijack.
			if service.Labels == nil {
				service.Labels = make(map[string]string)
			}
			service.Labels[sandboxLabel] = nameHash
			service.Spec.Selector = map[string]string{
				sandboxLabel: nameHash,
			}

			if err := ctrl.SetControllerReference(sandbox, service, r.Scheme); err != nil {
				return nil, fmt.Errorf("SetControllerReference for Service failed: %w", err)
			}
			if err := r.Update(ctx, service); err != nil {
				return nil, fmt.Errorf("failed to update service with owner reference: %w", err)
			}

		case resourceOwnedBySandbox:
			desiredSelector := map[string]string{
				sandboxLabel: nameHash,
			}
			patch := client.MergeFrom(service.DeepCopy())
			needsUpdate := false

			if service.Labels == nil {
				service.Labels = make(map[string]string)
			}
			if service.Labels[sandboxLabel] != nameHash {
				service.Labels[sandboxLabel] = nameHash
				needsUpdate = true
			}
			if !reflect.DeepEqual(service.Spec.Selector, desiredSelector) {
				service.Spec.Selector = desiredSelector
				needsUpdate = true
			}

			if needsUpdate {
				log.Info("Reconciling owned service drift", "Service.Namespace", service.Namespace, "Service.Name", service.Name, "Sandbox.Namespace", sandbox.Namespace, "Sandbox.Name", sandbox.Name)
				if err := r.Patch(ctx, service, patch); err != nil {
					return nil, fmt.Errorf("failed to patch owned service: %w", err)
				}
			}

		}

		r.setServiceStatus(sandbox, service)
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

	r.setServiceStatus(sandbox, service)
	return service, nil
}

// clearPodNameAnnotation removes the pod name annotation from the sandbox if it exists.
func (r *SandboxReconciler) clearPodNameAnnotation(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	if _, exists := sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]; !exists {
		return nil
	}
	log := log.FromContext(ctx)
	patch := client.MergeFrom(sandbox.DeepCopy())
	delete(sandbox.Annotations, sandboxv1alpha1.SandboxPodNameAnnotation)
	if err := r.Patch(ctx, sandbox, patch); err != nil {
		return fmt.Errorf("failed to clear pod name annotation: %w", err)
	}
	log.Info("Removed pod name annotation from sandbox", "Sandbox.Name", sandbox.Name)
	return nil
}

// setServiceStatus updates the sandbox status with the service name and FQDN.
func (r *SandboxReconciler) setServiceStatus(sandbox *sandboxv1alpha1.Sandbox, service *corev1.Service) {
	sandbox.Status.Service = service.Name
	sandbox.Status.ServiceFQDN = service.Name + "." + service.Namespace + ".svc." + r.ClusterDomain
}

func (r *SandboxReconciler) reconcilePod(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Pod, error) {
	log := log.FromContext(ctx)

	// Start a child span of ReconcileSandbox
	ctx, end := r.Tracer.StartSpan(ctx, nil, "reconcilePod", nil)
	defer end()

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
	podName := resolvePodName(sandbox)
	_, podNameAnnotationExists := sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]
	if podName != sandbox.Name {
		log.Info("Using tracked pod name from sandbox annotation", "podName", podName)
	}

	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: sandbox.Namespace}, pod)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get Pod")
			return nil, fmt.Errorf("pod get failed: %w", err)
		}
		if podNameAnnotationExists {
			log.Info("Pod referenced by annotation not found, clearing annotation to recover state", "podName", podName)
			if err := r.clearPodNameAnnotation(ctx, sandbox); err != nil {
				return nil, err
			}
		}
		pod = nil
	}

	if *sandbox.Spec.Replicas == 0 {
		if pod != nil {
			ownership, controllerRef := checkOwnership(pod, sandbox)
			switch ownership {
			case resourceOwnedBySandbox:
				if pod.DeletionTimestamp.IsZero() {
					log.Info("Deleting Pod because .Spec.Replicas is 0", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
					if err := r.Delete(ctx, pod); err != nil {
						return nil, fmt.Errorf("failed to delete pod: %w", err)
					}
				} else {
					log.Info("Pod is already being deleted", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
				}
			case resourceUnowned:
				log.Info("Refusing to delete pod: pod has no controllerRef pointing to this sandbox",
					"Pod.Name", pod.Name, "Sandbox.Name", sandbox.Name)
			case resourceOwnedByOther:
				log.Info("Refusing to delete pod: pod is owned by a different controller",
					"Pod.Name", pod.Name, "Sandbox.Name", sandbox.Name,
					"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)
			}
		}

		// Remove the pod name annotation from the sandbox if it exists
		if err := r.clearPodNameAnnotation(ctx, sandbox); err != nil {
			return nil, err
		}

		return nil, nil
	}

	ensurePodNameAnnotation := func(podName string) error {
		annotatedPodName := ""
		if sandbox.Annotations != nil {
			annotatedPodName = sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation]
		}

		if annotatedPodName == podName {
			return nil
		}

		if annotatedPodName != "" {
			log.Info("Skipping pod name annotation update because sandbox already tracks a different pod", "trackedPodName", annotatedPodName, "podName", podName)
			return nil
		}

		patch := client.MergeFrom(sandbox.DeepCopy())
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations[sandboxv1alpha1.SandboxPodNameAnnotation] = podName
		if err := r.Patch(ctx, sandbox, patch); err != nil {
			return fmt.Errorf("failed to set pod name annotation: %w", err)
		}

		return nil
	}

	reconcileExistingPod := func(pod *corev1.Pod) (*corev1.Pod, error) {
		log.Info("Found Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)

		if r.Tracer.IsRecording(ctx) {
			r.Tracer.AddEvent(ctx, "ExistingPodStatusObserved", map[string]string{
				"pod.Name":  pod.Name,
				"pod.Phase": string(pod.Status.Phase),
			})
		}

		ownership, controllerRef := checkOwnership(pod, sandbox)
		switch ownership {
		case resourceOwnedByOther:
			log.Info("Refusing to adopt pod: pod is owned by a different controller",
				"Pod.Name", pod.Name, "Sandbox.Name", sandbox.Name,
				"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)

			if err := r.clearPodNameAnnotation(ctx, sandbox); err != nil {
				return nil, err
			}

			return nil, fmt.Errorf("pod %q is owned by %s/%s (UID: %s), not by sandbox %q",
				pod.Name, controllerRef.Kind, controllerRef.Name, controllerRef.UID, sandbox.Name)

		case resourceUnowned:
			if err := ctrl.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
				return nil, fmt.Errorf("SetControllerReference for Pod failed: %w", err)
			}

		case resourceOwnedBySandbox:
			// No additional action needed — label applied below.
		}

		updated := r.updatePodMetadata(pod, sandbox, nameHash)
		if updated {
			if err := r.Update(ctx, pod); err != nil {
				return nil, fmt.Errorf("failed to update pod: %w", err)
			}
		}

		if err := ensurePodNameAnnotation(pod.Name); err != nil {
			return nil, err
		}

		// TODO - Do we enfore (change) spec if a pod exists ?
		// r.Patch(ctx, pod, client.Apply, client.ForceOwnership, client.FieldOwner("sandbox-controller"))
		return pod, nil
	}

	// 2. PATH: Existing Pod found (e.g., adopted from WarmPool or already exists)
	if pod != nil {
		return reconcileExistingPod(pod)
	}

	// Create new Pod
	log.Info("Creating a new Pod", "Pod.Namespace", sandbox.Namespace, "Pod.Name", sandbox.Name)
	labels := map[string]string{
		sandboxLabel: nameHash,
	}

	var managedLabelKeys []string
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Labels {
		labels[k] = v
		managedLabelKeys = append(managedLabelKeys, k)
	}
	annotations := map[string]string{}
	var managedAnnotationKeys []string
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Annotations {
		annotations[k] = v
		managedAnnotationKeys = append(managedAnnotationKeys, k)
	}
	slices.Sort(managedLabelKeys)
	slices.Sort(managedAnnotationKeys)
	if len(managedLabelKeys) > 0 {
		annotations[sandboxv1alpha1.SandboxPropagatedLabelsAnnotation] = strings.Join(managedLabelKeys, ",")
	}
	if len(managedAnnotationKeys) > 0 {
		annotations[sandboxv1alpha1.SandboxPropagatedAnnotationsAnnotation] = strings.Join(managedAnnotationKeys, ",")
	}

	mutatedSpec := sandbox.Spec.PodTemplate.Spec.DeepCopy()

	// Build PVC volumes from volumeClaimTemplates
	var pvcVolumes []corev1.Volume
	for _, pvcTemplate := range sandbox.Spec.VolumeClaimTemplates {
		pvcName := pvcTemplate.Name + "-" + sandbox.Name
		pvcVolumes = append(pvcVolumes, corev1.Volume{
			Name: pvcTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}
	mutatedSpec.Volumes = MergeVolumeClaimVolumes(mutatedSpec.Volumes, pvcVolumes)
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
		if k8serrors.IsAlreadyExists(err) {
			log.Info("Pod already exists, fetching existing pod",
				"Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
			existingPod := &corev1.Pod{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, existingPod); getErr != nil {
				return nil, fmt.Errorf("pod already exists but failed to fetch: %w", getErr)
			}
			return reconcileExistingPod(existingPod)
		}
		log.Error(err, "Failed to create", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		return nil, err
	}

	if err := ensurePodNameAnnotation(pod.Name); err != nil {
		return nil, err
	}

	if r.Tracer.IsRecording(ctx) {
		r.Tracer.AddEvent(ctx, "NewPodStatusObserved", map[string]string{
			"pod.Name":  pod.Name,
			"pod.Phase": string(pod.Status.Phase),
		})
	}

	return pod, nil
}

func (r *SandboxReconciler) updatePodMetadata(pod *corev1.Pod, sandbox *sandboxv1alpha1.Sandbox, nameHash string) bool {
	updated := false
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if pod.Labels[sandboxLabel] != nameHash {
		pod.Labels[sandboxLabel] = nameHash
		updated = true
	}
	// Propagate pod template labels to the existing pod (e.g., after warm pool adoption)
	var managedLabelKeys []string
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Labels {
		if pod.Labels[k] != v {
			pod.Labels[k] = v
			updated = true
		}
		managedLabelKeys = append(managedLabelKeys, k)
	}
	// Handle deletion of labels
	propagatedLabelsStr := pod.Annotations[sandboxv1alpha1.SandboxPropagatedLabelsAnnotation]
	if propagatedLabelsStr != "" {
		propagatedLabels := strings.Split(propagatedLabelsStr, ",")
		for _, k := range propagatedLabels {
			if k == "" {
				continue
			}
			if _, ok := sandbox.Spec.PodTemplate.ObjectMeta.Labels[k]; !ok {
				delete(pod.Labels, k)
				updated = true
			}
		}
	}
	// Propagate pod template annotations to the existing pod
	var managedAnnotationKeys []string
	if sandbox.Spec.PodTemplate.ObjectMeta.Annotations != nil {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Annotations {
			if pod.Annotations[k] != v {
				pod.Annotations[k] = v
				updated = true
			}
			managedAnnotationKeys = append(managedAnnotationKeys, k)
		}
	}
	// Handle deletion of annotations
	propagatedAnnotationsStr := pod.Annotations[sandboxv1alpha1.SandboxPropagatedAnnotationsAnnotation]
	if propagatedAnnotationsStr != "" {
		propagatedAnnotations := strings.Split(propagatedAnnotationsStr, ",")
		for _, k := range propagatedAnnotations {
			if k == "" {
				continue
			}
			if _, ok := sandbox.Spec.PodTemplate.ObjectMeta.Annotations[k]; !ok {
				delete(pod.Annotations, k)
				updated = true
			}
		}
	}
	// Update tracked annotations on the pod
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	slices.Sort(managedLabelKeys)
	newLabelsStr := strings.Join(managedLabelKeys, ",")
	if pod.Annotations[sandboxv1alpha1.SandboxPropagatedLabelsAnnotation] != newLabelsStr {
		pod.Annotations[sandboxv1alpha1.SandboxPropagatedLabelsAnnotation] = newLabelsStr
		updated = true
	}
	slices.Sort(managedAnnotationKeys)
	newAnnotationsStr := strings.Join(managedAnnotationKeys, ",")
	if pod.Annotations[sandboxv1alpha1.SandboxPropagatedAnnotationsAnnotation] != newAnnotationsStr {
		pod.Annotations[sandboxv1alpha1.SandboxPropagatedAnnotationsAnnotation] = newAnnotationsStr
		updated = true
	}
	return updated
}

func (r *SandboxReconciler) reconcilePVCs(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) error {
	log := log.FromContext(ctx)

	// Start a child span of ReconcileSandbox
	ctx, end := r.Tracer.StartSpan(ctx, nil, "reconcilePVCs", nil)
	defer end()

	for _, pvcTemplate := range sandbox.Spec.VolumeClaimTemplates {
		pvc := &corev1.PersistentVolumeClaim{}
		pvcName := pvcTemplate.Name + "-" + sandbox.Name
		err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: sandbox.Namespace}, pvc)
		if err == nil {
			ownership, controllerRef := checkOwnership(pvc, sandbox)
			switch ownership {
			case resourceOwnedByOther:
				log.Info("Refusing to use PVC: PVC is owned by a different controller",
					"PVC.Name", pvcName, "Sandbox.Name", sandbox.Name,
					"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)
				return fmt.Errorf("PVC %q is owned by %s/%s (UID: %s), not by sandbox %q",
					pvcName, controllerRef.Kind, controllerRef.Name, controllerRef.UID, sandbox.Name)

			case resourceUnowned:
				log.Info("Adopting unowned PVC", "PVC.Name", pvcName, "Sandbox.Name", sandbox.Name)
				if err := ctrl.SetControllerReference(sandbox, pvc, r.Scheme); err != nil {
					return fmt.Errorf("SetControllerReference for PVC failed: %w", err)
				}
				if err := r.Update(ctx, pvc); err != nil {
					return fmt.Errorf("failed to update PVC with owner reference: %w", err)
				}

			case resourceOwnedBySandbox:
				// Already owned by this sandbox — no action needed.
			}
			continue
		}

		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to get PVC")
			return fmt.Errorf("PVC Get Failed: %w", err)
		}

		pvcLabels := maps.Clone(pvcTemplate.Labels)
		if pvcLabels == nil {
			pvcLabels = make(map[string]string)
		}
		pvcLabels[sandboxLabel] = nameHash

		log.Info("Creating a new PVC", "PVC.Namespace", sandbox.Namespace, "PVC.Name", pvcName)
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pvcName,
				Namespace:   sandbox.Namespace,
				Annotations: maps.Clone(pvcTemplate.Annotations),
				Labels:      pvcLabels,
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
	}
	return nil
}

// handles sandbox expiry by deleting child resources and the sandbox itself if needed.
func (r *SandboxReconciler) handleSandboxExpiry(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) (bool, error) {
	log := log.FromContext(ctx)
	var allErrors error

	// Delete pod only if owned by this sandbox
	podName := resolvePodName(sandbox)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: sandbox.Namespace}, pod); err != nil {
		if !k8serrors.IsNotFound(err) {
			allErrors = errors.Join(allErrors, fmt.Errorf("failed to get pod: %w", err))
		}
	} else {
		ownership, controllerRef := checkOwnership(pod, sandbox)
		switch ownership {
		case resourceOwnedBySandbox:
			if err := r.Delete(ctx, pod); err != nil && !k8serrors.IsNotFound(err) {
				allErrors = errors.Join(allErrors, fmt.Errorf("failed to delete pod: %w", err))
			}
		case resourceUnowned:
			log.Info("Skipping pod deletion during expiry: pod has no controllerRef pointing to this sandbox",
				"Pod.Name", pod.Name, "Sandbox.Name", sandbox.Name)
		case resourceOwnedByOther:
			log.Info("Skipping pod deletion during expiry: pod is owned by a different controller",
				"Pod.Name", pod.Name, "Sandbox.Name", sandbox.Name,
				"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)
		}
	}

	// Delete service only if owned by this sandbox
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, service); err != nil {
		if !k8serrors.IsNotFound(err) {
			allErrors = errors.Join(allErrors, fmt.Errorf("failed to get service: %w", err))
		}
	} else {
		ownership, controllerRef := checkOwnership(service, sandbox)
		switch ownership {
		case resourceOwnedBySandbox:
			if err := r.Delete(ctx, service); err != nil && !k8serrors.IsNotFound(err) {
				allErrors = errors.Join(allErrors, fmt.Errorf("failed to delete service: %w", err))
			}
		case resourceUnowned:
			log.Info("Skipping service deletion during expiry: service has no controllerRef pointing to this sandbox",
				"Service.Name", service.Name, "Sandbox.Name", sandbox.Name)
		case resourceOwnedByOther:
			log.Info("Skipping service deletion during expiry: service is owned by a different controller",
				"Service.Name", service.Name, "Sandbox.Name", sandbox.Name,
				"Owner.Kind", controllerRef.Kind, "Owner.Name", controllerRef.Name, "Owner.UID", controllerRef.UID)
		}
	}

	if sandbox.Spec.ShutdownPolicy != nil && *sandbox.Spec.ShutdownPolicy == sandboxv1alpha1.ShutdownPolicyDelete {
		if err := r.Delete(ctx, sandbox); err != nil && !k8serrors.IsNotFound(err) {
			allErrors = errors.Join(allErrors, fmt.Errorf("failed to delete sandbox: %w", err))
		} else {
			return true, nil
		}
	}

	// If we reach here, sandbox is not deleted
	// Only update "expired" status if cleanup was successful
	if allErrors == nil {
		// Drop live-resource status while retaining terminal conditions.
		conditions := sandbox.Status.Conditions
		sandbox.Status = sandboxv1alpha1.SandboxStatus{Conditions: conditions}
		// Update status to mark as expired
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             sandboxv1alpha1.SandboxReasonExpired,
			Message:            "Sandbox has expired",
		})
	}

	return false, allErrors
}

// checks if the sandbox has expired
// returns true if expired, false otherwise
// if not expired, also returns the duration to requeue after.
func checkSandboxExpiry(sandbox *sandboxv1alpha1.Sandbox, now time.Time) (bool, time.Duration) {
	if sandbox.Spec.ShutdownTime == nil {
		return false, 0
	}
	shutdownTime := sandbox.Spec.ShutdownTime.Time
	if !now.Before(shutdownTime) {
		return true, 0
	}
	remainingTime := shutdownTime.Sub(now)

	// TODO(barney-s): Do we need a inverse exponential backoff here ?
	// requeueAfter := max(remainingTime/2, 2*time.Second)

	// Requeue at expiry time or in 2 seconds whichever is later
	requeueAfter := max(remainingTime, 2*time.Second)
	return false, requeueAfter
}

func setSandboxExpiredCondition(sandbox *sandboxv1alpha1.Sandbox) {
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             sandboxv1alpha1.SandboxReasonExpired,
		Message:            "Sandbox has expired",
	})
}

// sandboxMarkedExpired checks if the sandbox is already marked as expired.
func sandboxMarkedExpired(sandbox *sandboxv1alpha1.Sandbox) bool {
	cond := meta.FindStatusCondition(sandbox.Status.Conditions, string(sandboxv1alpha1.SandboxConditionReady))
	return cond != nil && (cond.Reason == sandboxv1alpha1.SandboxReasonExpired)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
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
		Owns(&corev1.Pod{}, builder.WithPredicates(labelSelectorPredicate)).
		Owns(&corev1.Service{}, builder.WithPredicates(labelSelectorPredicate)).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}
