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
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Key identifies a tracked sandbox in the registry.
type Key struct {
	Namespace string
	ClaimName string
}

// Client manages sandbox lifecycles and tracks active handles.
type Client struct {
	opts    Options
	k8s     *K8sHelper
	log     logr.Logger
	tracer  trace.Tracer
	svcName string

	mu              sync.Mutex
	registry        map[Key]*Sandbox
	stopSignal      context.CancelFunc // non-nil when signal handler is active
	stopAutoCleanup func()             // non-nil when auto-cleanup is enabled
	cleanupEnabled  bool               // tracks whether cleanup has been enabled
}

// NewClient creates a Client with shared configuration.
func NewClient(_ context.Context, opts Options) (*Client, error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}

	k8s := opts.K8sHelper
	if k8s == nil {
		var err error
		k8s, err = NewK8sHelper(opts.RestConfig, opts.Logger)
		if err != nil {
			return nil, err
		}
	}

	tracer, svcName := newTracer(opts)

	c := &Client{
		opts:     opts,
		k8s:      k8s,
		log:      opts.Logger,
		tracer:   tracer,
		svcName:  svcName,
		registry: make(map[Key]*Sandbox),
	}

	// Enable automatic cleanup if requested
	if opts.CleanupOnSignal {
		// Register signal handler for SIGINT/SIGTERM
		_ = c.EnableAutoCleanup()
	}

	return c, nil
}

// CreateSandbox provisions a new sandbox and returns a managed handle.
// On failure, the orphaned claim is cleaned up.
func (c *Client) CreateSandbox(ctx context.Context, warmPoolName, namespace string) (*Sandbox, error) {
	if warmPoolName == "" {
		return nil, fmt.Errorf("sandbox: warm pool name is required")
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	sandboxOpts := c.opts
	sandboxOpts.WarmPoolName = warmPoolName
	sandboxOpts.Namespace = namespace
	sandboxOpts.K8sHelper = c.k8s

	sb, err := New(ctx, sandboxOpts)
	if err != nil {
		return nil, err
	}

	if err := sb.Open(ctx); err != nil {
		return nil, err
	}

	key := Key{Namespace: namespace, ClaimName: sb.ClaimName()}
	// The registry key is built from the server-assigned (GenerateName) claim
	// name, which is unique to this sb, so a hit means a concurrent GetSandbox
	// attached to the very claim we just created and registered first.
	return c.trackOrAdoptRace(key, sb), nil
}

// GetSandbox retrieves an existing sandbox by claim name. Returns the
// cached handle if connected, otherwise re-attaches.
func (c *Client) GetSandbox(ctx context.Context, claimName, namespace string) (*Sandbox, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}
	key := Key{Namespace: namespace, ClaimName: claimName}

	c.mu.Lock()
	existing := c.registry[key]
	if existing != nil && existing.IsReady() {
		c.mu.Unlock()
		return existing, nil
	}
	// Evict stale handle atomically: we already know existing is nil or
	// not-ready, and no concurrent caller can have changed it since we
	// still hold the lock.
	if existing != nil {
		delete(c.registry, key)
	}
	c.mu.Unlock()

	sandboxOpts := c.opts
	sandboxOpts.Namespace = namespace
	sandboxOpts.K8sHelper = c.k8s

	// Verify claim exists and resolve sandbox name before constructing handle.
	if err := c.k8s.verifyClaimExists(ctx, claimName, namespace, c.tracer, c.svcName); err != nil {
		return nil, fmt.Errorf("sandbox: claim %q not found in %q: %w", claimName, namespace, err)
	}
	sandboxName, err := c.k8s.resolveSandboxName(ctx, claimName, namespace, sandboxOpts.SandboxReadyTimeout, c.tracer, c.svcName)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to resolve sandbox for claim %q: %w", claimName, err)
	}

	sb, err := New(ctx, sandboxOpts)
	if err != nil {
		return nil, err
	}

	// Inject identity so Open() takes the reconnect path.
	sb.mu.Lock()
	sb.claimName = claimName
	sb.sandboxName = sandboxName
	sb.mu.Unlock()

	if err := sb.Open(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: failed to re-attach to claim %q in %q: %w", claimName, namespace, err)
	}

	// A concurrent GetSandbox/CreateSandbox for the same key may have installed
	// a ready handle while we were attaching; adopt it if so.
	return c.trackOrAdoptRace(key, sb), nil
}

// trackOrAdoptRace resolves the registry race that both CreateSandbox and
// GetSandbox can hit: another goroutine may have installed a ready handle for
// key while sb was being opened/attached. Under the lock it re-checks for a
// tracked ready handle; if one exists it adopts that handle and tears down only
// sb's redundant transport (Disconnect), returning the tracked handle.
// Otherwise it registers sb and returns it.
//
// Disconnect (not Close) is deliberate: sb and the tracked handle reference the
// SAME shared claim, so Close would deleteClaim and destroy the sandbox out from
// under the handle we return. Disconnect runs on a detached context because the
// caller's ctx may already be cancelled/near-deadline, which would make
// Disconnect bail before tearing down the transport — leaking the very handle we
// discard.
func (c *Client) trackOrAdoptRace(key Key, sb *Sandbox) *Sandbox {
	c.mu.Lock()
	if tracked := c.registry[key]; tracked != nil && tracked.IsReady() {
		c.mu.Unlock()
		if derr := sb.Disconnect(context.Background()); derr != nil {
			c.log.Error(derr, "disconnecting redundant handle after registry race", "claim", key.ClaimName, "namespace", key.Namespace)
		}
		return tracked
	}
	c.registry[key] = sb
	c.mu.Unlock()
	return sb
}

// ListActiveSandboxes returns tracked sandboxes, pruning inactive handles.
func (c *Client) ListActiveSandboxes() []Key {
	c.mu.Lock()
	defer c.mu.Unlock()

	active := make([]Key, 0, len(c.registry))
	for key, sb := range c.registry {
		if !sb.IsReady() {
			delete(c.registry, key)
			continue
		}
		active = append(active, key)
	}
	return active
}

// ListAllSandboxes lists all SandboxClaim names in the given namespace.
func (c *Client) ListAllSandboxes(ctx context.Context, namespace string) ([]string, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}
	list, err := c.k8s.ExtensionsClient.SandboxClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to list claims in %q: %w", namespace, err)
	}
	names := make([]string, len(list.Items))
	for i := range list.Items {
		names[i] = list.Items[i].Name
	}
	return names, nil
}

// DeleteSandbox closes the handle (if tracked) and deletes the claim.
func (c *Client) DeleteSandbox(ctx context.Context, claimName, namespace string) error {
	if namespace == "" {
		namespace = defaultNamespace
	}
	key := Key{Namespace: namespace, ClaimName: claimName}

	c.mu.Lock()
	sb := c.registry[key]
	delete(c.registry, key)
	c.mu.Unlock()

	if sb != nil {
		return sb.Close(ctx)
	}
	return c.k8s.deleteClaim(ctx, claimName, namespace)
}

// DeleteAll closes and deletes all tracked sandboxes. Best-effort.
func (c *Client) DeleteAll(ctx context.Context) {
	c.mu.Lock()
	snapshot := make(map[Key]*Sandbox, len(c.registry))
	maps.Copy(snapshot, c.registry)
	c.registry = make(map[Key]*Sandbox)
	c.mu.Unlock()

	for key, sb := range snapshot {
		if err := sb.Close(ctx); err != nil {
			c.log.Error(err, "cleanup failed", "claim", key.ClaimName, "namespace", key.Namespace)
		}
	}
}

// EnableAutoCleanup calls DeleteAll on SIGINT/SIGTERM.
// Call the returned function to stop the signal handler.
func (c *Client) EnableAutoCleanup() (stop func()) {
	c.mu.Lock()
	if c.stopSignal != nil {
		c.mu.Unlock()
		return func() {}
	}
	c.cleanupEnabled = true
	ctx, cancel := context.WithCancel(context.Background())
	c.stopSignal = cancel
	c.mu.Unlock()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	// Create stop function that resets state
	stopFunc := func() {
		signal.Stop(ch)
		cancel()
		c.mu.Lock()
		c.cleanupEnabled = false
		c.stopSignal = nil
		c.stopAutoCleanup = nil
		c.mu.Unlock()
	}

	c.mu.Lock()
	c.stopAutoCleanup = stopFunc
	c.mu.Unlock()

	go func() {
		select {
		case sig := <-ch:
			// Stop signal handling immediately so subsequent signals use default behavior
			signal.Stop(ch)
			// Reset state so cleanup is not considered active
			c.mu.Lock()
			c.cleanupEnabled = false
			c.stopSignal = nil
			c.stopAutoCleanup = nil
			c.mu.Unlock()

			c.log.Info("signal received, cleaning up sandboxes", "signal", sig.String())
			// Run cleanup with bounded timeout to avoid blocking termination indefinitely
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			c.DeleteAll(cleanupCtx)

			// Re-raise so the default handler terminates the process.
			p, _ := os.FindProcess(os.Getpid())
			_ = p.Signal(sig)
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return stopFunc
}

// DisableAutoCleanup stops the automatic cleanup signal handler.
// Safe to call even if auto-cleanup was never enabled.
func (c *Client) DisableAutoCleanup() {
	c.mu.Lock()
	stop := c.stopAutoCleanup
	if stop != nil {
		c.stopAutoCleanup = nil
		c.stopSignal = nil
		c.cleanupEnabled = false
	}
	c.mu.Unlock()

	if stop != nil {
		stop()
	}
}
