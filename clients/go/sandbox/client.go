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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
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

// SandboxClient provides high-level interaction with an agent-sandbox instance.
type SandboxClient struct {
	opts Options
	log  logr.Logger

	agentsClient     agentsv1alpha1.AgentsV1alpha1Interface
	extensionsClient extensionsv1alpha1.ExtensionsV1alpha1Interface
	dynamicClient    dynamic.Interface
	coreClient       corev1client.CoreV1Interface
	discoveryClient  discoveryv1client.DiscoveryV1Interface
	restConfig       *rest.Config

	httpClient *http.Client
	baseURL    string

	claimName   string
	sandboxName string
	podName     string
	annotations map[string]string
	lastError   error // set on port-forward death; cleared on Open

	portForwardStopChan chan struct{}
	spdyUpgradeClient   *http.Client    // SPDY handshake client; tracked for cleanup on reconnect
	pfDialer            *trackingDialer // tracks SPDY connection for force-close on stuck ForwardPorts

	lifecycleSem chan struct{}      // capacity 1; serializes Open/Close with context cancellation
	mu           sync.Mutex         // protects state fields; never held across blocking I/O
	inflightOps  *sync.WaitGroup    // tracks in-flight operations for graceful Close; swapped on Close to avoid reuse panic
	backoffScale float64            // test hook: multiplier for backoff duration; 0 means 1.0
	openCancel   context.CancelFunc // set during Open; Close invokes to abort blocked waits; protected by mu

	tracer           trace.Tracer
	ownsGlobalTracer bool // true when EnableTracing auto-initialized the global TracerProvider
	traceServiceName string
	lifecycleSpan    trace.Span
	lifecycleCtx     context.Context
}

// NewClient creates a new SandboxClient with the given options.
// Call Open() to create a sandbox and establish connectivity.
func NewClient(opts Options) (*SandboxClient, error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}

	var ownsGlobalTracer bool
	if opts.EnableTracing && opts.TracerProvider == nil {
		if _, err := InitTracer(context.Background(), opts.TraceServiceName); err != nil {
			opts.Logger.Error(err, "failed to initialize tracing; continuing with noop tracer")
		} else {
			ownsGlobalTracer = true
		}
	}

	config := opts.RestConfig
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

	// Share a single HTTP client (and its connection pool) across all K8s
	// clientsets to avoid redundant connections to the API server.
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

	c := newClientFromInterfaces(opts, agentsCS.AgentsV1alpha1(), extensionsCS.ExtensionsV1alpha1(), dynClient, coreClient, discClient, config)
	c.ownsGlobalTracer = ownsGlobalTracer
	return c, nil
}

func newClientFromInterfaces(
	opts Options,
	agents agentsv1alpha1.AgentsV1alpha1Interface,
	extensions extensionsv1alpha1.ExtensionsV1alpha1Interface,
	dyn dynamic.Interface,
	core corev1client.CoreV1Interface,
	disc discoveryv1client.DiscoveryV1Interface,
	rc *rest.Config,
) *SandboxClient {
	transport := opts.HTTPTransport
	if transport == nil {
		transport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: opts.PerAttemptTimeout}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: opts.PerAttemptTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	c := &SandboxClient{
		opts:             opts,
		log:              opts.Logger,
		agentsClient:     agents,
		extensionsClient: extensions,
		dynamicClient:    dyn,
		coreClient:       core,
		discoveryClient:  disc,
		restConfig:       rc,
		httpClient: &http.Client{
			Transport: transport,
		},
		lifecycleSem: make(chan struct{}, 1),
		inflightOps:  &sync.WaitGroup{},
	}
	c.initTracer()
	return c
}

// Open creates a SandboxClaim and waits for the sandbox to become ready,
// then discovers the API URL based on the configured connection mode.
// If Open fails after creating the claim, it automatically cleans up
// the claim. If cleanup also fails, the error is joined and the caller
// should call Close() to retry deletion of the orphaned claim.
func (c *SandboxClient) Open(ctx context.Context) (retErr error) {
	select {
	case c.lifecycleSem <- struct{}{}:
		defer func() { <-c.lifecycleSem }()
	case <-ctx.Done():
		return fmt.Errorf("sandbox: open cancelled waiting for lifecycle lock: %w", ctx.Err())
	}

	// Cancellable context so Close() can abort blocked waits and reclaim the semaphore.
	openCtx, openCancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.openCancel = openCancel
	c.mu.Unlock()
	defer func() {
		openCancel()
		c.mu.Lock()
		c.openCancel = nil
		c.mu.Unlock()
	}()

	c.mu.Lock()
	if c.baseURL != "" {
		c.mu.Unlock()
		return ErrAlreadyOpen
	}
	if c.claimName != "" && c.sandboxName == "" {
		// Orphaned claim from a failed Open rollback or Close; caller must Close() first.
		c.mu.Unlock()
		return ErrOrphanedClaim
	}
	reconnect := c.claimName != ""
	c.lastError = nil
	c.mu.Unlock()

	// End any leftover lifecycle span from a prior Open (e.g. reconnect).
	c.mu.Lock()
	if c.lifecycleSpan != nil {
		c.lifecycleSpan.End()
	}
	// Detach from openCtx so lifecycleCtx outlives Open and parents spans until Close.
	lifecycleParent := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(openCtx))
	c.lifecycleCtx, c.lifecycleSpan = c.tracer.Start(lifecycleParent, c.traceServiceName+".lifecycle")
	c.mu.Unlock()

	defer func() {
		if retErr != nil {
			c.mu.Lock()
			if c.lifecycleSpan != nil {
				recordError(c.lifecycleSpan, retErr)
				c.lifecycleSpan.End()
				c.lifecycleSpan = nil
				c.lifecycleCtx = nil
			}
			c.mu.Unlock()
		}
	}()

	if reconnect {
		reconnCtx := openCtx
		if _, hasDeadline := openCtx.Deadline(); !hasDeadline {
			var reconnCancel context.CancelFunc
			reconnCtx, reconnCancel = context.WithTimeout(openCtx, c.opts.SandboxReadyTimeout)
			defer reconnCancel()
		}
		c.log.Info("reconnecting transport", "claim", c.ClaimName(), "sandbox", c.SandboxName())
		if err := c.verifyClaimExists(reconnCtx); err != nil {
			c.mu.Lock()
			if k8serrors.IsNotFound(err) {
				// Claim was deleted externally; reset to clean state so
				// the next Open() can create a fresh sandbox.
				c.claimName = ""
				c.sandboxName = ""
				c.podName = ""
				c.annotations = nil
				c.mu.Unlock()
				return fmt.Errorf("sandbox: claim was deleted; call Open() to create a new sandbox: %w", err)
			}
			// Cannot verify claim (transient error); keep orphaned state
			// so Close() can retry deletion.
			c.sandboxName = ""
			c.podName = ""
			c.annotations = nil
			c.mu.Unlock()
			return fmt.Errorf("%w: %w", ErrOrphanedClaim, err)
		}
		if err := c.verifySandboxAlive(reconnCtx); err != nil {
			c.mu.Lock()
			// Only clear sandbox identity on NotFound. Transient errors
			// preserve it so the next Open() can re-verify the same sandbox.
			if k8serrors.IsNotFound(err) {
				c.sandboxName = ""
				c.podName = ""
				c.annotations = nil
			}
			c.mu.Unlock()
			return fmt.Errorf("%w: %w", ErrOrphanedClaim, err)
		}
		c.httpClient.CloseIdleConnections() // clear stale connections from previous port-forward
		if err := c.discoverAPIURL(reconnCtx); err != nil {
			// Sandbox is alive; only transport failed. Keep state so
			// the caller can retry Open() without losing the sandbox.
			return fmt.Errorf("sandbox: reconnect transport failed (sandbox is alive; retry Open): %w", err)
		}
		return nil
	}

	if err := c.createClaim(openCtx); err != nil {
		return err
	}

	if err := c.waitForSandboxReady(openCtx); err != nil {
		return c.rollbackOpen(err)
	}

	if err := c.discoverAPIURL(openCtx); err != nil {
		return c.rollbackOpen(err)
	}

	return nil
}

// rollbackOpen deletes the claim using a detached context so cleanup
// succeeds even if the caller's ctx is already cancelled.
func (c *SandboxClient) rollbackOpen(originalErr error) error {
	c.mu.Lock()
	name := c.claimName
	c.stopPortForward()
	c.baseURL = ""
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), c.opts.CleanupTimeout)
	defer cancel()

	cleanupErr := c.deleteClaim(cleanupCtx)

	c.mu.Lock()
	c.sandboxName = ""
	c.podName = ""
	c.annotations = nil
	if cleanupErr == nil {
		c.claimName = ""
	}
	c.mu.Unlock()

	if cleanupErr != nil {
		c.log.Error(cleanupErr, "orphaned claim during Open rollback, could not delete", "claim", name)
		return errors.Join(originalErr, cleanupErr)
	}
	return originalErr
}

// Close deletes the SandboxClaim and cleans up resources.
// If the lifecycle lock is immediately available, cleanup proceeds
// regardless of ctx state. If a concurrent Open holds the lock,
// ctx can cancel the wait. Claim deletion always uses a detached
// context (CleanupTimeout) so cleanup succeeds even if ctx has expired.
func (c *SandboxClient) Close(ctx context.Context) error {
	// Cancel any in-progress Open so it releases the lifecycle semaphore.
	c.mu.Lock()
	if c.openCancel != nil {
		c.openCancel()
	}
	c.mu.Unlock()

	semCtx, semCancel := context.WithTimeout(context.Background(), c.opts.CleanupTimeout)
	defer semCancel()
	// Fast path: acquire immediately regardless of ctx state so cleanup
	// succeeds even when the caller's context is already cancelled.
	select {
	case c.lifecycleSem <- struct{}{}:
		defer func() { <-c.lifecycleSem }()
	default:
		// Semaphore held by concurrent Open. Honor the caller's context
		// so they can bail out, with a hard ceiling from CleanupTimeout.
		select {
		case c.lifecycleSem <- struct{}{}:
			defer func() { <-c.lifecycleSem }()
		case <-ctx.Done():
			return fmt.Errorf("sandbox: close cancelled waiting for lifecycle lock: %w", ctx.Err())
		case <-semCtx.Done():
			return fmt.Errorf("sandbox: close timed out waiting for lifecycle lock")
		}
	}

	// Immediately mark as not-ready so concurrent operations fail fast
	// with ErrNotReady instead of sending requests to a dying sandbox.
	// Swap inflightOps with a fresh WaitGroup so the drain goroutine
	// waits on the old one; new operations after Close use the new one,
	// avoiding a WaitGroup reuse panic if the drain times out.
	c.mu.Lock()
	c.stopPortForward()
	c.baseURL = ""
	drainWG := c.inflightOps
	c.inflightOps = &sync.WaitGroup{}
	c.mu.Unlock()

	// Wait for in-flight operations to finish before deleting the claim
	// so body reads are not interrupted by pod termination.
	drainDone := make(chan struct{})
	go func() {
		drainWG.Wait()
		close(drainDone)
	}()
	drainTimer := time.NewTimer(c.opts.CleanupTimeout)
	defer drainTimer.Stop()
	select {
	case <-drainDone:
	case <-drainTimer.C:
		c.log.Info("in-flight operations did not drain; proceeding with cleanup", "timeout", c.opts.CleanupTimeout)
	}

	// Always use a detached context for cleanup so it is not constrained
	// by the caller's deadline, which may be shorter than CleanupTimeout.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), c.opts.CleanupTimeout)
	defer cancel()
	err := c.deleteClaim(cleanupCtx)

	c.mu.Lock()
	c.sandboxName = ""
	c.podName = ""
	c.annotations = nil
	if err != nil && c.claimName != "" {
		c.log.Error(err, "orphaned claim during Close, could not delete; retry Close() to clean up", "claim", c.claimName)
	} else {
		c.claimName = ""
	}
	c.lastError = nil
	c.mu.Unlock()

	c.httpClient.CloseIdleConnections()

	c.mu.Lock()
	if c.lifecycleSpan != nil {
		if err != nil {
			recordError(c.lifecycleSpan, err)
		}
		c.lifecycleSpan.End()
		c.lifecycleSpan = nil
		c.lifecycleCtx = nil
	}
	c.mu.Unlock()

	// Flush buffered spans if this client initialized the global TracerProvider.
	// Full shutdown is the caller's responsibility at process exit via ShutdownTracer.
	if c.ownsGlobalTracer {
		globalProviderMu.Lock()
		p := globalProvider
		globalProviderMu.Unlock()
		if p != nil {
			flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer flushCancel()
			if flushErr := p.ForceFlush(flushCtx); flushErr != nil {
				c.log.Error(flushErr, "failed to flush trace spans; some spans may be lost")
			}
		}
	}

	return err
}

// IsReady returns true if the sandbox is ready for communication.
func (c *SandboxClient) IsReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL != ""
}

// ClaimName returns the name of the SandboxClaim.
func (c *SandboxClient) ClaimName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claimName
}

// SandboxName returns the name of the provisioned Sandbox.
func (c *SandboxClient) SandboxName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sandboxName
}

// PodName returns the name of the underlying pod.
func (c *SandboxClient) PodName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.podName
}

// Annotations returns a copy of the Sandbox resource's annotations.
func (c *SandboxClient) Annotations() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.annotations == nil {
		return nil
	}
	cp := make(map[string]string, len(c.annotations))
	for k, v := range c.annotations {
		cp[k] = v
	}
	return cp
}

// errPrefix returns a sandbox-identity-qualified prefix for error messages.
func (c *SandboxClient) errPrefix() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimName != "" {
		return fmt.Sprintf("sandbox[%s/%s]", c.opts.Namespace, c.claimName)
	}
	return "sandbox"
}

// trackOp increments the in-flight operation counter and returns a
// function that decrements it. Used as `defer c.trackOp()()` in each
// public operation so Close can drain in-flight work before deleting
// the claim. If the client is not ready (baseURL cleared by Close),
// returns a no-op to avoid racing with the drain's WaitGroup.Wait.
func (c *SandboxClient) trackOp() func() {
	c.mu.Lock()
	if c.baseURL == "" {
		c.mu.Unlock()
		return func() {}
	}
	wg := c.inflightOps
	wg.Add(1)
	c.mu.Unlock()
	return wg.Done
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

func (c *SandboxClient) createClaim(ctx context.Context) (retErr error) {
	ctx, span := c.startSpan(ctx, "create_claim")
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	randBytes := make([]byte, 4)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("sandbox: failed to generate random claim name: %w", err)
	}
	name := fmt.Sprintf("sandbox-claim-%s", hex.EncodeToString(randBytes))
	span.SetAttributes(AttrClaimName.String(name))

	// Propagate W3C trace context via claim annotations.
	var annotations map[string]string
	if traceCtx := traceContextJSON(ctx); traceCtx != "" {
		annotations = map[string]string{
			"opentelemetry.io/trace-context": traceCtx,
		}
	}

	claim := &extv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.opts.Namespace,
			Annotations: annotations,
		},
		Spec: extv1alpha1.SandboxClaimSpec{
			TemplateRef: extv1alpha1.SandboxTemplateRef{
				Name: c.opts.TemplateName,
			},
		},
	}

	_, err := c.extensionsClient.SandboxClaims(c.opts.Namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("%w: template=%s namespace=%s: %w", ErrClaimFailed, c.opts.TemplateName, c.opts.Namespace, err)
	}

	c.mu.Lock()
	c.claimName = name
	c.mu.Unlock()

	c.log.Info("claim created", "claim", name, "namespace", c.opts.Namespace)
	return nil
}

func (c *SandboxClient) waitForSandboxReady(ctx context.Context) (retErr error) {
	ctx, span := c.startSpan(ctx, "wait_for_sandbox_ready")
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	ctx, cancel := context.WithTimeout(ctx, c.opts.SandboxReadyTimeout)
	defer cancel()

	c.mu.Lock()
	name := c.claimName
	c.mu.Unlock()

	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second
	var lastConditions string

	for {
		list, listErr := c.agentsClient.Sandboxes(c.opts.Namespace).List(ctx, listOpts)
		if listErr == nil {
			for i := range list.Items {
				if isSandboxReady(&list.Items[i]) {
					c.setSandboxState(&list.Items[i])
					c.log.Info("sandbox ready", "claim", name, "sandbox", list.Items[i].Name, "pod", c.PodName())
					return nil
				}
				lastConditions = formatConditions(list.Items[i].Status.Conditions)
			}
			listOpts.ResourceVersion = list.ResourceVersion
		} else {
			c.log.V(1).Info("list sandboxes failed, falling through to watch", "error", listErr, "claim", name)
		}

		watcher, err := c.agentsClient.Sandboxes(c.opts.Namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: sandbox did not become ready within %s (last conditions: %s)", ErrTimeout, c.opts.SandboxReadyTimeout, lastConditions)
			}
			// Transient watch creation error; backoff and re-list.
			c.log.V(1).Info("watch creation failed, retrying", "error", err, "claim", name)
			listOpts.ResourceVersion = ""
			c.sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		done, watchErr := c.drainSandboxWatch(ctx, watcher, &lastConditions)
		watcher.Stop()
		if done {
			c.log.Info("sandbox ready", "claim", name, "sandbox", c.SandboxName(), "pod", c.PodName())
			return nil
		}
		if watchErr != nil {
			return watchErr
		}
		c.log.V(1).Info("sandbox watch closed, re-establishing", "claim", name)
		listOpts.ResourceVersion = ""

		c.sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

// drainSandboxWatch returns (true, nil) when the sandbox is ready,
// (false, err) on fatal error, or (false, nil) if the watch channel closes.
func (c *SandboxClient) drainSandboxWatch(ctx context.Context, watcher watch.Interface, lastConditions *string) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("%w: sandbox did not become ready within %s (last conditions: %s)", ErrTimeout, c.opts.SandboxReadyTimeout, *lastConditions)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return false, nil // watch closed, caller should retry
			}
			if event.Type == watch.Error {
				// All watch errors (410 Gone, transient 5xx, etc.) are
				// recoverable: return nil so the outer loop re-lists with
				// backoff rather than aborting and deleting the claim.
				c.log.V(1).Info("transient watch error, will re-list", "error", event.Object)
				return false, nil
			}
			if event.Type == watch.Deleted {
				return false, ErrSandboxDeleted
			}
			sb, ok := event.Object.(*sandboxv1alpha1.Sandbox)
			if !ok {
				continue
			}
			*lastConditions = formatConditions(sb.Status.Conditions)
			if isSandboxReady(sb) {
				c.setSandboxState(sb)
				return true, nil
			}
		}
	}
}

func isSandboxReady(sb *sandboxv1alpha1.Sandbox) bool {
	for _, cond := range sb.Status.Conditions {
		if cond.Type == string(sandboxv1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *SandboxClient) setSandboxState(sb *sandboxv1alpha1.Sandbox) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sandboxName = sb.Name
	if sb.Annotations != nil {
		c.annotations = make(map[string]string, len(sb.Annotations))
		for k, v := range sb.Annotations {
			c.annotations[k] = v
		}
	} else {
		c.annotations = nil
	}
	if name, ok := sb.Annotations[PodNameAnnotation]; ok {
		c.podName = name
	} else {
		c.podName = sb.Name
	}
}

// verifyClaimExists checks that the SandboxClaim still exists.
func (c *SandboxClient) verifyClaimExists(ctx context.Context) error {
	c.mu.Lock()
	name := c.claimName
	c.mu.Unlock()

	_, err := c.extensionsClient.SandboxClaims(c.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: claim %s no longer exists: %w", name, err)
	}
	return nil
}

// verifySandboxAlive checks that the sandbox pod still exists and is ready.
func (c *SandboxClient) verifySandboxAlive(ctx context.Context) error {
	c.mu.Lock()
	name := c.sandboxName
	c.mu.Unlock()

	sb, err := c.agentsClient.Sandboxes(c.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: pod no longer available for %s: %w", name, err)
	}
	if !isSandboxReady(sb) {
		return fmt.Errorf("sandbox: %s is no longer ready (conditions: %s)", name, formatConditions(sb.Status.Conditions))
	}
	c.setSandboxState(sb)
	return nil
}

func (c *SandboxClient) discoverAPIURL(ctx context.Context) error {
	var mode string
	var err error
	switch {
	case c.opts.APIURL != "":
		c.mu.Lock()
		c.baseURL = c.opts.APIURL
		c.mu.Unlock()
		mode = "direct"
	case c.opts.GatewayName != "":
		mode = "gateway"
		err = c.waitForGatewayIP(ctx)
	default:
		mode = "port-forward"
		err = c.startPortForward(ctx)
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	url := c.baseURL
	c.mu.Unlock()
	c.log.Info("API URL discovered", "url", url, "mode", mode)
	return nil
}

func (c *SandboxClient) deleteClaim(ctx context.Context) error {
	c.mu.Lock()
	name := c.claimName
	c.mu.Unlock()

	if name == "" {
		return nil
	}
	err := c.extensionsClient.SandboxClaims(c.opts.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.log.V(1).Info("claim already deleted (404)", "claim", name)
			c.mu.Lock()
			c.claimName = ""
			c.mu.Unlock()
			return nil
		}
		return fmt.Errorf("sandbox: failed to delete claim %s: %w", name, err)
	}

	c.mu.Lock()
	c.claimName = ""
	c.mu.Unlock()
	c.log.Info("claim deleted", "claim", name)
	return nil
}

// stopPortForward requires c.mu to be held.
func (c *SandboxClient) stopPortForward() {
	if c.portForwardStopChan != nil {
		close(c.portForwardStopChan)
		c.portForwardStopChan = nil
	}
	// Force-close the SPDY connection to unblock a stuck ForwardPorts goroutine.
	if c.pfDialer != nil {
		c.pfDialer.Close()
		c.pfDialer = nil
	}
	if c.spdyUpgradeClient != nil {
		c.spdyUpgradeClient.CloseIdleConnections()
		c.spdyUpgradeClient = nil
	}
}
