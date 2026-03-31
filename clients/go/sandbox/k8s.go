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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	agentsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/typed/api/v1alpha1"
	extensionsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/typed/api/v1alpha1"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// sandboxState holds the identity metadata returned when a sandbox becomes ready.
type sandboxState struct {
	SandboxName string
	PodName     string
	Annotations map[string]string
}

// K8sHelper encapsulates all Kubernetes API interactions for sandbox lifecycle
// management. It can be shared across multiple Sandbox instances.
type K8sHelper struct {
	AgentsClient     agentsv1alpha1.AgentsV1alpha1Interface
	ExtensionsClient extensionsv1alpha1.ExtensionsV1alpha1Interface
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
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
		DynamicClient:    dynClient,
		CoreClient:       coreClient,
		DiscoveryClient:  discClient,
		RestConfig:       config,
		Log:              log,
	}, nil
}

// createClaim creates a SandboxClaim and returns its generated name.
func (h *K8sHelper) createClaim(ctx context.Context, namespace, templateName string, tracer trace.Tracer, svcName string) (string, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "create_claim")
	defer span.End()

	var annotations map[string]string
	if traceCtx := traceContextJSON(ctx); traceCtx != "" {
		annotations = map[string]string{
			"opentelemetry.io/trace-context": traceCtx,
		}
	}

	claim := &extv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sandbox-claim-",
			Namespace:    namespace,
			Annotations:  annotations,
		},
		Spec: extv1alpha1.SandboxClaimSpec{
			TemplateRef: extv1alpha1.SandboxTemplateRef{
				Name: templateName,
			},
		},
	}

	created, err := h.ExtensionsClient.SandboxClaims(namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		recordError(span, err)
		return "", fmt.Errorf("%w: template=%s namespace=%s: %w", ErrClaimFailed, templateName, namespace, err)
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

// waitForSandboxReady watches the Sandbox resource until it becomes ready,
// returning the sandbox state. The caller must provide a context with an
// appropriate timeout.
func (h *K8sHelper) waitForSandboxReady(ctx context.Context, claimName, namespace string, timeout time.Duration, tracer trace.Tracer, svcName string) (*sandboxState, error) {
	ctx, span := startSpan(ctx, tracer, svcName, "wait_for_sandbox_ready")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", claimName),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second
	var lastConditions string

	for {
		list, listErr := h.AgentsClient.Sandboxes(namespace).List(ctx, listOpts)
		if listErr == nil {
			for i := range list.Items {
				if list.Items[i].Name != claimName {
					continue
				}
				if isSandboxReady(&list.Items[i]) {
					state := extractState(&list.Items[i])
					return state, nil
				}
				lastConditions = formatConditions(list.Items[i].Status.Conditions)
			}
			listOpts.ResourceVersion = list.ResourceVersion
		} else {
			h.Log.V(1).Info("list sandboxes failed, falling through to watch", "error", listErr, "claim", claimName)
		}

		watcher, err := h.AgentsClient.Sandboxes(namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				retErr := fmt.Errorf("%w: sandbox did not become ready within %s (last conditions: %s)", ErrTimeout, timeout, lastConditions)
				recordError(span, retErr)
				return nil, retErr
			}
			h.Log.V(1).Info("watch creation failed, retrying", "error", err, "claim", claimName)
			listOpts.ResourceVersion = ""
			sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		state, done, watchErr := h.drainSandboxWatch(ctx, watcher, claimName, timeout, &lastConditions)
		watcher.Stop()
		if done {
			return state, nil
		}
		if watchErr != nil {
			recordError(span, watchErr)
			return nil, watchErr
		}
		h.Log.V(1).Info("sandbox watch closed, re-establishing", "claim", claimName)
		listOpts.ResourceVersion = ""

		sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

func (h *K8sHelper) drainSandboxWatch(ctx context.Context, watcher watch.Interface, claimName string, timeout time.Duration, lastConditions *string) (*sandboxState, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, false, fmt.Errorf("%w: sandbox did not become ready within %s (last conditions: %s)", ErrTimeout, timeout, *lastConditions)
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
			sb, ok := event.Object.(*sandboxv1alpha1.Sandbox)
			if !ok {
				continue
			}
			if sb.Name != claimName {
				continue
			}
			*lastConditions = formatConditions(sb.Status.Conditions)
			if isSandboxReady(sb) {
				return extractState(sb), true, nil
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
		retErr := fmt.Errorf("sandbox: claim %s no longer exists: %w", name, err)
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

func isSandboxReady(sb *sandboxv1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func extractState(sb *sandboxv1alpha1.Sandbox) *sandboxState {
	state := &sandboxState{
		SandboxName: sb.Name,
	}
	if sb.Annotations != nil {
		state.Annotations = make(map[string]string, len(sb.Annotations))
		for k, v := range sb.Annotations {
			state.Annotations[k] = v
		}
	}
	if name, ok := sb.Annotations[PodNameAnnotation]; ok {
		state.PodName = name
	} else {
		state.PodName = sb.Name
	}
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
