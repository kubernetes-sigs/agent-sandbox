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

package managedsandbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/pool"
)

const (
	sandboxLabel                       = "agents.x-k8s.io/sandbox-name-hash"
	managedSandboxControllerFieldOwner = "managed-sandbox-controller"
	indexHostPodName                   = "status.host.podName"
)

// ManagedSandboxReconciler reconciles a Sandbox object.
type ManagedSandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Tracer asmetrics.Instrumenter

	PodAgentImage string

	// PoolProvisioner creates pool pods on demand for multi-tenant sandboxes
	// (sandbox.Spec.Image set). Must be non-nil for any multi-tenant Sandbox
	// to reconcile successfully; legacy (podTemplate) sandboxes do not need it.
	poolProvisioner *pool.Provisioner

	// PoolAgents is the connection cache used to talk to each pool pod's
	// gRPC pod-agent. Must be non-nil whenever PoolProvisioner is.
	poolAgents *pool.AgentClientPool

	// GatewayParent, when set, makes the controller materialize an HTTPRoute
	// per multi-tenant sandbox pointing at the controller-wide Gateway named
	// here. Nil disables HTTPRoute generation.
	GatewayParent *gwv1.ParentReference
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ManagedSandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *ManagedSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	sandbox := &sandboxv1alpha1.ManagedSandbox{}
	if err := r.Get(ctx, req.NamespacedName, sandbox); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("sandbox resource not found. Ignoring since object must be deleted")
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

	// If the sandbox is being deleted, do nothing here. Legacy (podTemplate)
	// sandboxes rely on owner-ref cascade for child cleanup; multi-tenant
	// sandboxes leak their bubblewrap tenant until the periodic GC sweep
	// reaps it (see internal/pool.GC). This avoids the operational pain of
	// finalizers (stuck deletes when the controller or pod-agent is down).
	if !sandbox.DeletionTimestamp.IsZero() {
		logger.Info("Sandbox is being deleted")
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

	// Snapshot the original object so we can compute a status patch at
	// the end. Using MergeFrom (vs Status().Update) avoids resource-
	// version conflicts when the informer cache is briefly stale after
	// our previous Status write.
	originalSandbox := sandbox.DeepCopy()
	var err error
	result := ctrl.Result{}

	var childResult ctrl.Result
	childResult, err = r.reconcileMultiTenant(ctx, sandbox)
	result.RequeueAfter = childResult.RequeueAfter

	// Update status
	if statusUpdateErr := r.updateStatus(ctx, originalSandbox, sandbox); statusUpdateErr != nil {
		// Surface update error
		err = errors.Join(err, statusUpdateErr)
	}
	// return errors seen
	return result, err
}

// updateStatus writes status changes to the API server using a merge
// patch against `original`. Patch (not Update) avoids resource-version
// conflicts when the informer cache is briefly stale after our previous
// Status write — that 409 was the cause of repeated "object has been
// modified" errors in the logs.
func (r *ManagedSandboxReconciler) updateStatus(ctx context.Context, original, sandbox *sandboxv1alpha1.ManagedSandbox) error {
	log := log.FromContext(ctx)

	if reflect.DeepEqual(&original.Status, &sandbox.Status) {
		return nil
	}

	patch := client.MergeFrom(original)
	if err := r.Status().Patch(ctx, sandbox, patch); err != nil {
		log.Error(err, "Failed to patch sandbox status")
		return err
	}
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

// SetupWithManager sets up the controller with the Manager.
func (r *ManagedSandboxReconciler) SetupWithManager(mgr ctrl.Manager, concurrentWorkers int) error {
	if err := r.init(mgr); err != nil {
		return err
	}

	// Field index for Status.Host.PodName so the pool-pod Watch's
	// enqueueSandboxesForPoolPod resolves in O(matches) instead of
	// scanning every ManagedSandbox in the namespace on every pool-pod
	// event.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&sandboxv1alpha1.ManagedSandbox{},
		indexHostPodName,
		func(obj client.Object) []string {
			s := obj.(*sandboxv1alpha1.ManagedSandbox)
			if s.Status.Host == nil || s.Status.Host.PodName == "" {
				return nil
			}
			return []string{s.Status.Host.PodName}
		},
	); err != nil {
		return fmt.Errorf("index ManagedSandbox by %s: %w", indexHostPodName, err)
	}

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

	// Pool pods are not owned by any Sandbox CR (one pool pod serves many
	// tenants), so Owns() doesn't reach them. Watch them explicitly and
	// enqueue every Sandbox bound to that pod via Status.Host.PodName.
	poolPodPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{pool.LabelManagedBy: pool.LabelManagedByValue},
	})
	if err != nil {
		return err
	}

	// Register a fast-delete side handler on the Sandbox informer. When a
	// multi-tenant Sandbox is deleted we get one DeleteEvent here (with
	// the last-cached object intact), dispatch a best-effort pod-agent
	// DeleteSandbox, and forget about it. The periodic pool.GC sweep
	// covers anything we miss (controller crashed, pod-agent unreachable
	// at delete time, ...). No finalizer.
	sbInformer, err := mgr.GetCache().GetInformer(context.Background(), &sandboxv1alpha1.ManagedSandbox{})
	if err != nil {
		return fmt.Errorf("get sandbox informer: %w", err)
	}
	if _, err := sbInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		DeleteFunc: r.handleSandboxDelete,
	}); err != nil {
		return fmt.Errorf("add sandbox delete handler: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.ManagedSandbox{}).
		Owns(&corev1.Pod{}, builder.WithPredicates(labelSelectorPredicate)).
		Owns(&corev1.Service{}, builder.WithPredicates(labelSelectorPredicate)).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueSandboxesForPoolPod),
			builder.WithPredicates(poolPodPredicate),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentWorkers}).
		Complete(r)
}

func (r *ManagedSandboxReconciler) init(mgr ctrl.Manager) error {
	// TODO: these should be configurable
	const poolPVCStorage = "10Gi"
	const poolPbVCStorageClass = "" // default
	const poolCapacity = 10

	storage, err := resource.ParseQuantity(poolPVCStorage)
	if err != nil {
		return fmt.Errorf("invalid pool PVC storage: %w", err)
	}

	r.poolProvisioner = &pool.Provisioner{
		Client:        mgr.GetClient(),
		FieldOwner:    managedSandboxControllerFieldOwner,
		CreateService: r.GatewayParent != nil,
		Builder: pool.PodBuilder{
			AgentImage:      r.PodAgentImage,
			Capacity:        poolCapacity,
			PVCStorage:      storage,
			PVCStorageClass: poolPbVCStorageClass,
		},
	}
	r.poolAgents = &pool.AgentClientPool{}
	// Pool GC reaps orphan bubblewrap tenants on pool pods whose
	// corresponding Sandbox CR has been deleted. Runs once on boot then
	// every pool.DefaultGCInterval. Leader-elected so multi-replica
	// controllers don't fan out duplicate sweeps.
	gc := &pool.GC{Client: mgr.GetClient(), Agents: r.poolAgents}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		gc.RunForever(ctx, pool.DefaultGCInterval)
		return nil
	})); err != nil {
		return fmt.Errorf("register pool GC: %w", err)
	}
	return nil
}

// enqueueSandboxesForPoolPod returns reconcile requests for every Sandbox
// bound to the given pool pod via Status.Host.PodName. Backed by the
// `indexHostPodName` field index registered in SetupWithManager — the
// List below hits an in-memory map keyed by pod name, not a full scan.
func (r *ManagedSandboxReconciler) enqueueSandboxesForPoolPod(ctx context.Context, obj client.Object) []ctrl.Request {
	list := &sandboxv1alpha1.ManagedSandboxList{}
	if err := r.List(ctx, list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{indexHostPodName: obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		reqs = append(reqs, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name},
		})
	}
	return reqs
}
