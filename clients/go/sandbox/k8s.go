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

package sandbox

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	agentsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/typed/api/v1beta1"
	extensionsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/typed/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const clientAnnotation = "agents.x-k8s.io/client-first-requested-at"

// sandboxState holds the identity metadata returned when a sandbox becomes ready.
type sandboxState struct {
	SandboxName string
	PodName     string
	PodIP       string
	ServiceFQDN string
	Annotations map[string]string
}

// K8sHelper encapsulates all Kubernetes API interactions for sandbox lifecycle
// management. It can be shared across multiple Sandbox instances.
type K8sHelper struct {
	AgentsClient     agentsv1beta1.AgentsV1beta1Interface
	ExtensionsClient extensionsv1beta1.ExtensionsV1beta1Interface
	DynamicClient    dynamic.Interface
	CoreClient       corev1client.CoreV1Interface
	DiscoveryClient  discoveryv1client.DiscoveryV1Interface
	RestConfig       *rest.Config

	Log logr.Logger
}

// NewK8sHelper creates a K8sHelper by loading kubeconfig and constructing
// all required clientsets. If restConfig is non-nil it is used directly;
// otherwise in-cluster config is tried first, then ~/.kube/config.
func NewK8sHelper(restConfig *rest.Config, log logr.Logger) (*K8sHelper, error) {
	config := restConfig
	if config == nil {
		var err error
		config, err = rest.InClusterConfig()
		if err != nil {
			config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(),
				&clientcmd.ConfigOverrides{},
			).ClientConfig()
			if err != nil {
				return nil, fmt.Errorf("sandbox: failed to load kubeconfig: %w", err)
			}
		}
	}

	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create shared HTTP client: %w", err)
	}

	agentsCS, err := agentsclientset.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create agents clientset: %w", err)
	}

	extensionsCS, err := extensionsclientset.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create extensions clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create dynamic client: %w", err)
	}

	coreClient, err := corev1client.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create core client: %w", err)
	}

	discClient, err := discoveryv1client.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create discovery client: %w", err)
	}

	return &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1beta1(),
		ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		DynamicClient:    dynClient,
		CoreClient:       coreClient,
		DiscoveryClient:  discClient,
		RestConfig:       config,
		Log:              log,
	}, nil
}

// stampClientRequestTime records the client-side request start time on the
// claim annotations, allocating the map if needed. An existing value is left
// untouched: callers may stamp the true start of the request earlier in their
// own stack, and that is more accurate than the moment we build the claim.
// This mirrors the Python clients, which also only set the key when absent.
func stampClientRequestTime(annotations map[string]string, now time.Time) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, ok := annotations[clientAnnotation]; !ok {
		annotations[clientAnnotation] = now.UTC().Format(time.RFC3339Nano)
	}
	return annotations
}

// claimLifecycle returns the lifecycle that expires a claim ttl after now, or
// nil when ttl is unset. The API keeps whole seconds and truncates, so the
// deadline is rounded up to keep a short ttl from expiring early.
func claimLifecycle(now time.Time, ttl time.Duration) *extv1beta1.Lifecycle {
	if ttl <= 0 {
		return nil
	}
	shutdownTime := metav1.NewTime(now.Add(ttl).Add(time.Second - 1).Truncate(time.Second))
	return &extv1beta1.Lifecycle{
		ShutdownTime:   &shutdownTime,
		ShutdownPolicy: extv1beta1.ShutdownPolicyDelete,
	}
}

// createClaim creates a SandboxClaim and returns its generated name.
func (h *K8sHelper) createClaim(ctx context.Context, namespace, warmPoolName string, env []extv1beta1.EnvVar, labels map[string]string, shutdownAfter time.Duration, tracer trace.Tracer, svcName string) (string, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "create_claim")
	defer span.End()

	var annotations map[string]string
	if traceCtx := traceContextJSON(ctx); traceCtx != "" {
		annotations = map[string]string{
			"opentelemetry.io/trace-context": traceCtx,
		}
	}
	now := time.Now()
	annotations = stampClientRequestTime(annotations, now)

	claimLabels := make(map[string]string, len(labels)+1)
	maps.Copy(claimLabels, labels)
	claimLabels[sandboxv1beta1.CreatedByLabel] = "go-client"

	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sandbox-claim-",
			Namespace:    namespace,
			Annotations:  annotations,
			Labels:       claimLabels,
		},
		Spec: extv1beta1.SandboxClaimSpec{
			WarmPoolRef: extv1beta1.SandboxWarmPoolRef{
				Name: warmPoolName,
			},
			Env:       env,
			Lifecycle: claimLifecycle(now, shutdownAfter),
		},
	}

	created, err := h.ExtensionsClient.SandboxClaims(namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		recordError(span, err)
		return "", fmt.Errorf("%w: warmpool=%s namespace=%s: %w", ErrClaimFailed, warmPoolName, namespace, err)
	}

	name := created.Name
	span.SetAttributes(AttrClaimName.String(name))
	h.Log.Info("claim created", "claim", name, "namespace", namespace)
	return name, nil
}

// deleteClaim deletes a SandboxClaim. Returns nil if the claim is already gone.
func (h *K8sHelper) deleteClaim(ctx context.Context, name, namespace string) error {
	if name == "" {
		return nil
	}
	err := h.ExtensionsClient.SandboxClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.V(1).Info("claim already deleted (404)", "claim", name)
			return nil
		}
		return fmt.Errorf("sandbox: failed to delete claim %s: %w", name, err)
	}
	h.Log.Info("claim deleted", "claim", name)
	return nil
}

// terminalReadyReasons are Ready=False reasons that the claim and Sandbox
// controllers do not retry on their own (see computeReadyCondition in
// extensions/controllers/sandboxclaim_controller.go). Transient reasons such as
// AdoptionPending, SandboxMissing, SandboxNotReady and ReconcilerError are
// left out because the controller recovers from them. Kept in sync with
// TERMINAL_CLAIM_READY_REASONS in the Python and TypeScript SDKs.
var terminalReadyReasons = map[string]bool{
	"InvalidMetadata":                                true,
	"EnvVarsInjectionRejected":                       true,
	"VolumeClaimTemplatesError":                      true,
	extv1beta1.ClaimExpiredReason:                    true,
	sandboxv1beta1.SandboxReasonExpired:              true,
	sandboxv1beta1.SandboxReasonInvalidConfiguration: true,
	sandboxv1beta1.SandboxReasonPodFailed:            true,
	sandboxv1beta1.SandboxReasonPodSucceeded:         true,
}

// currentNotReady returns the Ready=False condition, or nil if Ready is absent,
// not False, or describes an older generation than the object's current spec.
func currentNotReady(conditions []metav1.Condition, generation int64) *metav1.Condition {
	ready := meta.FindStatusCondition(conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionFalse {
		return nil
	}
	if ready.ObservedGeneration != 0 && ready.ObservedGeneration < generation {
		return nil
	}
	return ready
}

// terminalFailure returns ErrClaimFailed if ready carries a terminal reason.
func terminalFailure(kind, name string, ready *metav1.Condition) error {
	if ready == nil || !terminalReadyReasons[ready.Reason] {
		return nil
	}
	return fmt.Errorf("%w: %s %s has terminal reason %s: %s", ErrClaimFailed, kind, name, ready.Reason, ready.Message)
}

// claimFailure returns an error if the claim reports a failure that waiting
// longer cannot fix, so callers fail fast instead of burning the full timeout.
func claimFailure(claim *extv1beta1.SandboxClaim) error {
	ready := currentNotReady(claim.Status.Conditions, claim.Generation)
	if ready == nil {
		return nil
	}
	switch ready.Reason {
	case "TemplateNotFound":
		return fmt.Errorf("%w: claim %s: %s", ErrTemplateNotFound, claim.Name, ready.Message)
	case "WarmPoolNotFound":
		return fmt.Errorf("%w: claim %s: %s", ErrWarmPoolNotFound, claim.Name, ready.Message)
	}
	return terminalFailure("claim", claim.Name, ready)
}

// sandboxFailure is claimFailure for the Sandbox itself, which reports
// terminal reasons (PodFailed, SandboxExpired, ...) only after the claim has
// already resolved to its name.
func sandboxFailure(sb *sandboxv1beta1.Sandbox) error {
	return terminalFailure("sandbox", sb.Name, currentNotReady(sb.Status.Conditions, sb.Generation))
}

// resolveSandboxName watches SandboxClaim status until the sandbox name is
// populated. With warm pool, the sandbox name may differ from the claim name.
// It returns early with a claimFailure error if the claim can never become ready.
func (h *K8sHelper) resolveSandboxName(ctx context.Context, claimName, namespace string, timeout time.Duration, tracer trace.Tracer, svcName string) (string, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "resolve_sandbox_name")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", claimName),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second

	for {
		claim, err := h.ExtensionsClient.SandboxClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err == nil {
			if failure := claimFailure(claim); failure != nil {
				recordError(span, failure)
				return "", failure
			}
			if name := claim.Status.SandboxStatus.Name; name != "" {
				h.Log.Info("sandbox name resolved", "claim", claimName, "sandbox", name)
				return name, nil
			}
		} else if k8serrors.IsNotFound(err) {
			retErr := fmt.Errorf("%w: claim %s deleted during name resolution", ErrSandboxDeleted, claimName)
			recordError(span, retErr)
			return "", retErr
		} else {
			h.Log.V(1).Info("get claim failed, falling through to watch", "error", err, "claim", claimName)
		}

		watcher, err := h.ExtensionsClient.SandboxClaims(namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				retErr := fmt.Errorf("%w: sandbox name not resolved for claim %s within %s", ErrTimeout, claimName, timeout)
				recordError(span, retErr)
				return "", retErr
			}
			h.Log.V(1).Info("claim watch creation failed, retrying", "error", err, "claim", claimName)
			sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		name, done, watchErr := h.drainClaimWatch(ctx, watcher, claimName, timeout)
		watcher.Stop()
		if done {
			h.Log.Info("sandbox name resolved", "claim", claimName, "sandbox", name)
			return name, nil
		}
		if watchErr != nil {
			recordError(span, watchErr)
			return "", watchErr
		}
		h.Log.V(1).Info("claim watch closed, re-establishing", "claim", claimName)

		sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

func (h *K8sHelper) drainClaimWatch(ctx context.Context, watcher watch.Interface, claimName string, timeout time.Duration) (string, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return "", false, fmt.Errorf("%w: sandbox name not resolved for claim %s within %s", ErrTimeout, claimName, timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", false, nil
			}
			if event.Type == watch.Error {
				h.Log.V(1).Info("transient claim watch error, will re-list", "error", event.Object)
				return "", false, nil
			}
			if event.Type == watch.Deleted {
				return "", false, fmt.Errorf("%w: claim %s deleted during name resolution", ErrSandboxDeleted, claimName)
			}
			claim, ok := event.Object.(*extv1beta1.SandboxClaim)
			if !ok {
				continue
			}
			if failure := claimFailure(claim); failure != nil {
				return "", false, failure
			}
			if name := claim.Status.SandboxStatus.Name; name != "" {
				return name, true, nil
			}
		}
	}
}

// WaitForSandboxReady waits until the named Sandbox has a true Ready condition.
// sandboxName is the backing Sandbox name, not the SandboxClaim name. It does not
// connect to the runtime. Use a context deadline to bound the wait; cancellation
// and deadline errors are detectable with errors.Is. A missing Sandbox is waited
// for, while deletion observed during the watch returns ErrSandboxDeleted.
// A terminal Ready=False reason such as PodFailed returns ErrClaimFailed.
// API list/watch failures are retried until ctx ends.
func (h *K8sHelper) WaitForSandboxReady(ctx context.Context, sandboxName, namespace string) error {
	if sandboxName == "" || namespace == "" {
		return fmt.Errorf("sandbox: sandbox name and namespace are required")
	}
	_, err := h.waitForSandboxState(ctx, sandboxName, namespace)
	return err
}

// waitForSandboxReady adds SDK tracing and timeout semantics to the shared wait.
func (h *K8sHelper) waitForSandboxReady(ctx context.Context, sandboxName, namespace string, timeout time.Duration, tracer trace.Tracer, svcName string) (*sandboxState, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "wait_for_sandbox_ready")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	state, err := h.waitForSandboxState(ctx, sandboxName, namespace)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: sandbox %s did not become ready within %s: %v", ErrTimeout, sandboxName, timeout, err)
		}
		recordError(span, err)
	}
	return state, err
}

// waitForSandboxState shares the list/watch loop between SDK opens and public waits.
func (h *K8sHelper) waitForSandboxState(ctx context.Context, sandboxName, namespace string) (*sandboxState, error) {
	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", sandboxName),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second
	var lastConditions string

	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("sandbox %s readiness wait ended (last conditions: %s): %w", sandboxName, lastConditions, err)
		}
		list, listErr := h.AgentsClient.Sandboxes(namespace).List(ctx, listOpts)
		if listErr == nil {
			for i := range list.Items {
				if list.Items[i].Name != sandboxName {
					continue
				}
				if isSandboxReady(&list.Items[i]) {
					return extractState(&list.Items[i]), nil
				}
				if failure := sandboxFailure(&list.Items[i]); failure != nil {
					return nil, failure
				}
				lastConditions = formatConditions(list.Items[i].Status.Conditions)
			}
			listOpts.ResourceVersion = list.ResourceVersion
		} else {
			h.Log.V(1).Info("list sandboxes failed, falling through to watch", "error", listErr, "sandbox", sandboxName)
		}

		watcher, err := h.AgentsClient.Sandboxes(namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				retErr := fmt.Errorf("sandbox %s readiness wait ended (last conditions: %s): %w", sandboxName, lastConditions, ctx.Err())
				return nil, retErr
			}
			h.Log.V(1).Info("watch creation failed, retrying", "error", err, "sandbox", sandboxName)
			listOpts.ResourceVersion = ""
			sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		state, done, watchErr := h.drainSandboxWatch(ctx, watcher, sandboxName, &lastConditions)
		watcher.Stop()
		if done {
			return state, nil
		}
		if watchErr != nil {
			return nil, watchErr
		}
		h.Log.V(1).Info("sandbox watch closed, re-establishing", "sandbox", sandboxName)
		listOpts.ResourceVersion = ""

		sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

func (h *K8sHelper) drainSandboxWatch(ctx context.Context, watcher watch.Interface, sandboxName string, lastConditions *string) (*sandboxState, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, false, fmt.Errorf("sandbox %s readiness wait ended (last conditions: %s): %w", sandboxName, *lastConditions, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, false, nil
			}
			if event.Type == watch.Error {
				h.Log.V(1).Info("transient watch error, will re-list", "error", event.Object)
				return nil, false, nil
			}
			if event.Type == watch.Deleted {
				return nil, false, ErrSandboxDeleted
			}
			sb, ok := event.Object.(*sandboxv1beta1.Sandbox)
			if !ok {
				continue
			}
			if sb.Name != sandboxName {
				continue
			}
			*lastConditions = formatConditions(sb.Status.Conditions)
			if isSandboxReady(sb) {
				return extractState(sb), true, nil
			}
			if failure := sandboxFailure(sb); failure != nil {
				return nil, false, failure
			}
		}
	}
}

// verifyClaimExists checks that the SandboxClaim still exists.
func (h *K8sHelper) verifyClaimExists(ctx context.Context, name, namespace string, tracer trace.Tracer, svcName string) error {
	ctx, span := startSpan(ctx, tracer, svcName, "verify_claim_exists")
	defer span.End()

	_, err := h.ExtensionsClient.SandboxClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		retErr := fmt.Errorf("%w: claim %s: %w", ErrSandboxDeleted, name, err)
		recordError(span, retErr)
		return retErr
	}
	return nil
}

// verifySandboxAlive checks that the sandbox still exists and is ready,
// returning updated state.
func (h *K8sHelper) verifySandboxAlive(ctx context.Context, name, namespace string, tracer trace.Tracer, svcName string) (*sandboxState, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "verify_sandbox_alive")
	defer span.End()

	sb, err := h.AgentsClient.Sandboxes(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		retErr := fmt.Errorf("sandbox: pod no longer available for %s: %w", name, err)
		recordError(span, retErr)
		return nil, retErr
	}
	if !isSandboxReady(sb) {
		retErr := fmt.Errorf("sandbox: %s is no longer ready (conditions: %s)", name, formatConditions(sb.Status.Conditions))
		recordError(span, retErr)
		return nil, retErr
	}
	return extractState(sb), nil
}

func isSandboxReady(sb *sandboxv1beta1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1beta1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func extractState(sb *sandboxv1beta1.Sandbox) *sandboxState {
	state := &sandboxState{
		SandboxName: sb.Name,
	}
	if sb.Annotations != nil {
		state.Annotations = make(map[string]string, len(sb.Annotations))
		maps.Copy(state.Annotations, sb.Annotations)
	}
	if name, ok := sb.Annotations[PodNameAnnotation]; ok && name != "" {
		state.PodName = name
	} else {
		state.PodName = sb.Name
	}
	state.PodIP = selectPodIP(sb.Status.PodIPs)
	state.ServiceFQDN = sb.Status.ServiceFQDN
	return state
}

func formatConditions(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return "none observed"
	}
	var b strings.Builder
	for i, cond := range conditions {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(cond.Type)
		b.WriteByte('=')
		b.WriteString(string(cond.Status))
	}
	return b.String()
}
