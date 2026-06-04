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

package managedsandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/examples/managed-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/examples/managed-sandbox/internal/pool"
)

// shortRequeue is used when we want the next reconcile to fire promptly
// but cannot rely on an external K8s event (e.g. we just cleared
// Status.Host so the next reconcile picks a different pool pod). A small
// non-zero RequeueAfter is the modern controller-runtime equivalent of
// the now-deprecated Result{Requeue: true}.
const shortRequeue = 100 * time.Millisecond

// podAgentRPCTimeout bounds each pod-agent gRPC call so a stuck pool pod
// can't wedge the controller's reconcile worker.
const podAgentRPCTimeout = 10 * time.Second

// CreateSandbox retry/give-up budget.
//
//   - createSandboxRetryInterval — gap between attempts when the pod-agent
//     keeps failing CreateSandbox (transient).
//   - createSandboxRetryBudget   — once the first failure is older than
//     this, we stop trying. Time anchor is the LastTransitionTime of the
//     `Created` Status condition: it's set to False/CreateSandboxFailed
//     on the first failure (Status flips from absent to False, so K8s
//     stamps LastTransitionTime) and preserved across same-reason calls.
//
// Numbers chosen as the comment in the prior reviewing pass suggested:
// "retry in 5s, stop after 1m, at least 2 attempts" — 60s / 5s ≥ 2.
const (
	createSandboxRetryInterval = 5 * time.Second
	createSandboxRetryBudget   = 60 * time.Second
)

const (
	reasonCreateSandboxFailed  = "CreateSandboxFailed"
	reasonCreateSandboxTimeout = "CreateSandboxTimeout"
	reasonSandboxCreated       = "SandboxCreated"
)

// reconcileMultiTenant is the multi-tenant counterpart of
// reconcileChildResources. It is invoked when sandbox.Spec.Image is set.
//
// Responsibilities:
//   - Pick (or provision) a pool pod whose base image matches Spec.Image.
//   - Persist the binding to Status.Host so subsequent reconciles are sticky.
//   - Dial the pod-agent on the chosen pod and ensure the bubblewrap
//     sandbox exists, mirroring its observed state into Status.Conditions.
//
// In this scaffolding pass the pod-agent dial is stubbed: we only resolve
// the binding and set a "not yet wired" Ready condition. The remaining
// integration is intentionally left as TODO so this can land as small,
// reviewable changes.
func (r *ManagedSandboxReconciler) reconcileMultiTenant(ctx context.Context, sandbox *sandboxv1alpha1.ManagedSandbox) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	// Terminal-state short-circuit. Once we've given up on CreateSandbox
	// the only way out is deletion + recreate; further reconciles must
	// not retry (and must not overwrite the terminal Reason).
	if finished := meta.FindStatusCondition(sandbox.Status.Conditions,
		string(sandboxv1alpha1.ManagedSandboxConditionFinished)); finished != nil &&
		finished.Status == metav1.ConditionTrue &&
		finished.Reason == reasonCreateSandboxTimeout {
		return ctrl.Result{}, nil
	}

	// 1. Pick a pool pod (sticky if Status.Host.PodName already set).
	selector := &pool.Selector{Client: r.Client}
	podName, err := selector.Choose(ctx, sandbox)
	switch {
	case err == nil:
		// got a pod
	case errors.Is(err, pool.ErrNoCapacity):
		log.Info("No pool pod with free capacity; provisioning a new one",
			"Sandbox.Name", sandbox.Name, "Image", sandbox.Spec.Image.Reference)
		podName, err = r.poolProvisioner.CreateNew(ctx, sandbox.Namespace, sandbox.Spec.Image.Reference)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("pool: ensure: %w", err)
		}
	case errors.Is(err, pool.ErrWaitingForPoolPod):
		// A pool pod exists but isn't ready. Wait for it rather than
		// provisioning a duplicate. Pool-pod Watch only enqueues bound
		// Sandboxes, so we (unbound) need to poll.
		log.Info("Pool pod warming up; waiting",
			"Sandbox.Name", sandbox.Name, "Image", sandbox.Spec.Image.Reference)
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             "WaitingForPoolPod",
			Message:            "Pool pod is warming up; will retry",
		})
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("pool: selector: %w", err)
	}

	// 2. Persist binding.
	if err := r.bindToPool(ctx, sandbox, podName); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Pod readiness gate.
	pod, err := pool.GetPod(ctx, r.Client, sandbox.Namespace, podName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("pool: get pod %s: %w", podName, err)
	}
	if pod == nil {
		// Bound pool pod is gone. If we have its PVC name in status, try
		// to recreate the pod with the same name + PVC binding — that
		// preserves any sandbox state still on the PVC. The PVC carries
		// the OCI image-ref annotation we stamped at provision time, so
		// we can reconstruct the pod without re-reading the Sandbox spec
		// (which may have drifted but doesn't matter — the existing
		// sandboxes on the PVC were built against the original image).
		pvcName := ""
		if sandbox.Status.Host != nil {
			pvcName = sandbox.Status.Host.PVCName
		}
		if pvcName != "" {
			if err := r.poolProvisioner.RecreatePod(ctx, sandbox.Namespace, pvcName); err == nil {
				log.Info("Recreated pool pod from existing PVC",
					"Sandbox.Name", sandbox.Name, "Pool.Pod", pvcName)
				return ctrl.Result{RequeueAfter: shortRequeue}, nil
			} else if !apierrors.IsNotFound(errors.Unwrap(err)) && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("recreate pool pod: %w", err)
			}
			log.Info("PVC also gone; rescheduling onto a fresh pool pod",
				"Sandbox.Name", sandbox.Name, "Pool.Pod", podName)
		}
		log.Info("Bound pool pod is gone; clearing binding",
			"Sandbox.Name", sandbox.Name, "Pool.Pod", podName)
		r.clearPoolBinding(sandbox, "PoolPodMissing",
			"Pool pod "+podName+" was not found; will reschedule onto another pool pod")
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}

	// 4. Dial the pod-agent and ensure the sandbox exists. Wait until the
	// pod has an IP AND is Ready (pod-agent's gRPC health probe has
	// passed) — dialing earlier would burn the CreateSandbox retry
	// budget on i/o timeouts. The pool-pod Watch re-enqueues us when
	// the pod's status changes.
	if pod.Status.PodIP == "" {
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             "PoolPodNoIP",
			Message:            "Pool pod " + podName + " has no IP yet",
		})
		return ctrl.Result{}, nil
	}
	if !pool.IsPodReady(pod) {
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             "PoolPodNotReady",
			Message:            "Pool pod " + podName + " is not Ready yet",
		})
		return ctrl.Result{}, nil
	}
	agent, err := r.poolAgents.For(ctx, podName, pod.Status.PodIP)
	if err != nil {
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             "PodAgentDialFailed",
			Message:            err.Error(),
		})
		// Return the error so controller-runtime's rate limiter applies
		// exponential backoff. No K8s event would re-enqueue us otherwise.
		return ctrl.Result{}, fmt.Errorf("pod-agent dial: %w", err)
	}
	workspacePath := "/home"
	if ws := sandbox.Spec.Workspace; ws != nil && ws.MountPath != "" {
		workspacePath = ws.MountPath
	}
	sshToken, sshSecretName, err := r.ensureSSHToken(ctx, sandbox)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ssh token: %w", err)
	}
	sandbox.Status.SSHSecretName = sshSecretName
	// PodIP for now: per-pool-pod Service DNS would be cleaner but is
	// out-of-scope until external Gateway/NodePort exposure lands.
	sandbox.Status.SSHHost = pod.Status.PodIP
	sandbox.Status.SSHPort = sshPort

	rpcCtx, cancel := context.WithTimeout(ctx, podAgentRPCTimeout)
	defer cancel()
	handle, err := agent.CreateSandbox(rpcCtx, pool.CreateSandboxRequest{
		UID:                string(sandbox.UID),
		Name:               sandbox.Namespace + "/" + sandbox.Name,
		ImageReference:     sandbox.Spec.Image.Reference,
		WorkspaceMountPath: workspacePath,
		SessionToken:       sshToken,
	})
	if err != nil {
		// ResourceExhausted: this pool pod is full (per pod-agent's count,
		// which may diverge from our selector's count after a crash). Drop
		// the binding so the next reconcile picks a different pool pod.
		// Requeue explicitly so we don't depend on the status-write
		// triggering a watch (a future status-filter predicate could
		// suppress it).
		// TODO: make sure the same pod is not chosen repeatedly if it keeps reporting ResourceExhausted.
		if status.Code(err) == codes.ResourceExhausted {
			log.Info("Pod-agent reports capacity exhausted; clearing binding",
				"Sandbox.Name", sandbox.Name, "Pool.Pod", podName)
			r.clearPoolBinding(sandbox, "PodAgentAtCapacity", err.Error())
			return ctrl.Result{RequeueAfter: shortRequeue}, nil
		}
		return r.handleCreateSandboxFailure(sandbox, err)
	}
	// Success: stamp the Created condition. The Status flip from
	// False/absent → True resets LastTransitionTime, retiring the retry
	// budget anchor without any explicit clearing.
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.ManagedSandboxConditionCreated),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: sandbox.Generation,
		Reason:             reasonSandboxCreated,
		Message:            "Pod-agent created the sandbox",
	})
	r.applySandboxPhase(sandbox, handle.State)

	// 5. HTTPRoute (best-effort, no-op if Gateway not configured).
	if err := r.reconcileHTTPRoute(ctx, sandbox); err != nil {
		return ctrl.Result{}, fmt.Errorf("httproute: %w", err)
	}
	return ctrl.Result{}, nil
}

// bindToPool writes Status.Host if absent, capturing pod and PVC binding.
// Idempotent: if PodName already matches, only PodUID/NodeName/PVCName are
// refreshed when missing.
func (r *ManagedSandboxReconciler) bindToPool(ctx context.Context, sandbox *sandboxv1alpha1.ManagedSandbox, podName string) error {
	if sandbox.Status.Host == nil {
		sandbox.Status.Host = &sandboxv1alpha1.SandboxHost{}
	}
	if sandbox.Status.Host.PodName != "" && sandbox.Status.Host.PodName != podName {
		return fmt.Errorf("pool: sandbox already bound to pod %q, refusing to rebind to %q",
			sandbox.Status.Host.PodName, podName)
	}
	sandbox.Status.Host.PodName = podName

	pod, err := pool.GetPod(ctx, r.Client, sandbox.Namespace, podName)
	if err != nil {
		return err
	}
	if pod != nil {
		newUID := string(pod.UID)
		// If the pod was replaced (delete + RecreatePod, or controller
		// migrated us to a different pool pod), reset the CreateSandbox
		// retry budget so the new pod-agent gets a full 60s window
		// rather than inheriting the previous pod's timer.
		if sandbox.Status.Host.PodUID != "" && sandbox.Status.Host.PodUID != newUID {
			meta.RemoveStatusCondition(&sandbox.Status.Conditions,
				string(sandboxv1alpha1.ManagedSandboxConditionCreated))
		}
		sandbox.Status.Host.PodUID = newUID
		sandbox.Status.Host.NodeName = pod.Spec.NodeName
		// Resolve PVC backing /var/lib/sandboxes.
		for _, v := range pod.Spec.Volumes {
			if v.Name == "sandbox-state" && v.PersistentVolumeClaim != nil {
				sandbox.Status.Host.PVCName = v.PersistentVolumeClaim.ClaimName
				break
			}
		}
	}
	return nil
}

// handleSandboxDelete is fired by the Sandbox informer the moment a CR
// is removed from etcd. The DeleteEvent carries the last-cached object
// so we still have `Status.Host.PodName` etc. We dispatch a single
// best-effort `DeleteSandbox` to the pod-agent and forget about it —
// the periodic pool.GC sweep is the durable backup if this fails.
//
// We deliberately do NOT use a finalizer; finalizers are operationally
// painful when the controller or pod-agent is degraded.
func (r *ManagedSandboxReconciler) handleSandboxDelete(obj any) {
	sb, _ := extractDeletedSandbox(obj)
	if sb == nil {
		return
	}
	if sb.Status.Host == nil || sb.Status.Host.PodName == "" {
		return
	}

	podName := sb.Status.Host.PodName
	uid := string(sb.UID)
	namespace := sb.Namespace
	expectedPodUID := sb.Status.Host.PodUID

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), podAgentRPCTimeout)
		defer cancel()
		log := log.FromContext(ctx).WithName("fast-delete").
			WithValues("Sandbox.UID", uid, "Pool.Pod", podName)

		pod, err := pool.GetPod(ctx, r.Client, namespace, podName)
		if err != nil {
			log.Error(err, "get pool pod")
			return
		}
		if pod == nil || pod.Status.PodIP == "" {
			log.V(1).Info("pool pod gone; sandbox runtime already gone")
			r.poolAgents.Forget(podName)
			return
		}
		if expectedPodUID != "" && string(pod.UID) != expectedPodUID {
			log.V(1).Info("pool pod UID changed; sandbox runtime already gone")
			r.poolAgents.Forget(podName)
			return
		}
		agent, err := r.poolAgents.For(ctx, podName, pod.Status.PodIP)
		if err != nil {
			log.Error(err, "dial pod-agent (GC sweep will retry)")
			return
		}
		if err := agent.DeleteSandbox(ctx, uid); err != nil {
			log.Error(err, "DeleteSandbox (GC sweep will retry)")
		}
	}()
}

// extractDeletedSandbox unwraps the various shapes the informer can pass
// to a DeleteFunc: the typed object directly, or a
// cache.DeletedFinalStateUnknown wrapper used when the watch was
// disconnected and the delete was inferred.
func extractDeletedSandbox(obj any) (*sandboxv1alpha1.ManagedSandbox, bool) {
	if sb, ok := obj.(*sandboxv1alpha1.ManagedSandbox); ok {
		return sb, true
	}
	if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		if sb, ok := d.Obj.(*sandboxv1alpha1.ManagedSandbox); ok {
			return sb, true
		}
	}
	return nil, false
}

// handleCreateSandboxFailure decides whether to retry pod-agent
// CreateSandbox or surface a terminal failure.
//
// The retry budget anchor is the `Created` condition's
// `LastTransitionTime`: the first failure flips Status from absent → False
// (so K8s stamps the time); subsequent failures with the same Status and
// Reason preserve it. Once the elapsed time exceeds
// `createSandboxRetryBudget` we set a terminal `Finished=true
// Reason=CreateSandboxTimeout` and stop requeueing — the Sandbox CR
// must be deleted and recreated to try again.
//
// Returns a fixed-interval requeue (not an error) so the rate limiter's
// exponential backoff doesn't extend the budget unpredictably.
func (r *ManagedSandboxReconciler) handleCreateSandboxFailure(
	sandbox *sandboxv1alpha1.ManagedSandbox, createErr error,
) (ctrl.Result, error) {
	existing := meta.FindStatusCondition(sandbox.Status.Conditions,
		string(sandboxv1alpha1.ManagedSandboxConditionCreated))

	// If we have a prior Created=False/CreateSandboxFailed condition and
	// it's older than the budget, give up.
	if existing != nil &&
		existing.Status == metav1.ConditionFalse &&
		existing.Reason == reasonCreateSandboxFailed &&
		time.Since(existing.LastTransitionTime.Time) >= createSandboxRetryBudget {
		elapsed := time.Since(existing.LastTransitionTime.Time).Truncate(time.Second)
		msg := fmt.Sprintf("Giving up after %s: %s", elapsed, createErr.Error())
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionCreated),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             reasonCreateSandboxTimeout,
			Message:            msg,
		})
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: sandbox.Generation,
			Reason:             reasonCreateSandboxTimeout,
			Message:            msg,
		})
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionFinished),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: sandbox.Generation,
			Reason:             reasonCreateSandboxTimeout,
			Message:            msg,
		})
		return ctrl.Result{}, nil
	}

	// Budget not exhausted: record the failure (LastTransitionTime is
	// stamped on first call, preserved on subsequent same-Status+Reason
	// calls per meta.SetStatusCondition semantics) and retry.
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.ManagedSandboxConditionCreated),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             reasonCreateSandboxFailed,
		Message:            createErr.Error(),
	})
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             reasonCreateSandboxFailed,
		Message:            createErr.Error(),
	})
	return ctrl.Result{RequeueAfter: createSandboxRetryInterval}, nil
}

// clearPoolBinding drops Status.Host (and resets SSH endpoint fields) so
// the next reconcile lets the Selector pick a fresh pool pod. Used when
// the bound pod has gone away or has signaled it can't serve us
// (ResourceExhausted, etc.).
func (r *ManagedSandboxReconciler) clearPoolBinding(sandbox *sandboxv1alpha1.ManagedSandbox, reason, message string) {
	sandbox.Status.Host = nil
	sandbox.Status.SSHHost = ""
	sandbox.Status.SSHPort = 0
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// applySandboxPhase mirrors the pod-agent's observed sandbox phase into
// the Sandbox CR's Ready/Finished conditions.
func (r *ManagedSandboxReconciler) applySandboxPhase(sandbox *sandboxv1alpha1.ManagedSandbox, state pool.SandboxState) {
	ready := metav1.Condition{
		Type:               string(sandboxv1alpha1.ManagedSandboxConditionReady),
		ObservedGeneration: sandbox.Generation,
	}
	switch state.Phase {
	case pool.PhaseRunning:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "SandboxRunning"
		ready.Message = "Sandbox is running"
	case pool.PhaseCreating:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "SandboxCreating"
		ready.Message = "Sandbox is starting"
	case pool.PhaseStopping:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "SandboxStopping"
		ready.Message = "Sandbox is stopping"
	case pool.PhaseStopped, pool.PhaseFailed:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "SandboxTerminal"
		ready.Message = "Sandbox has exited: " + state.Reason
	default:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "SandboxUnknown"
		ready.Message = state.Message
	}
	meta.SetStatusCondition(&sandbox.Status.Conditions, ready)

	if state.Phase == pool.PhaseStopped || state.Phase == pool.PhaseFailed {
		finished := metav1.Condition{
			Type:               string(sandboxv1alpha1.ManagedSandboxConditionFinished),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: sandbox.Generation,
			Reason:             state.Reason,
			Message:            state.Message,
		}
		meta.SetStatusCondition(&sandbox.Status.Conditions, finished)
	} else {
		meta.RemoveStatusCondition(&sandbox.Status.Conditions, string(sandboxv1alpha1.ManagedSandboxConditionFinished))
	}
}
