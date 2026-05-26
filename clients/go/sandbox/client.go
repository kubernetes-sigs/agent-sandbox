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
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var ErrClosed = errors.New("sandbox: client is closed")

const maxCleanupConcurrency = 10

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

	mu          sync.Mutex
	registry    map[Key]*Sandbox
	closed      bool
	stopSignal  context.CancelFunc // non-nil when signal handler is active
	cleanupStop func()             // stores the cancel/stop callback from EnableAutoCleanup
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

	if opts.CleanupOnSignal {
		_ = c.EnableAutoCleanup()
	}

	return c, nil
}

// CreateSandbox provisions a new sandbox and returns a managed handle.
// On failure, the orphaned claim is cleaned up.
func (c *Client) CreateSandbox(ctx context.Context, template, namespace string) (*Sandbox, error) {
	if template == "" {
		return nil, fmt.Errorf("sandbox: template name is required")
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.mu.Unlock()

	sandboxOpts := c.opts
	sandboxOpts.TemplateName = template
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = sb.Close(ctx)
		return nil, ErrClosed
	}
	c.registry[key] = sb
	c.mu.Unlock()

	return sb, nil
}

// GetSandbox retrieves an existing sandbox by claim name. Returns the
// cached handle if connected, otherwise re-attaches.
func (c *Client) GetSandbox(ctx context.Context, claimName, namespace string) (*Sandbox, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}
	key := Key{Namespace: namespace, ClaimName: claimName}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	existing := c.registry[key]
	c.mu.Unlock()

	if existing != nil && existing.IsReady() {
		return existing, nil
	}

	// Evict stale handle.
	if existing != nil {
		c.mu.Lock()
		delete(c.registry, key)
		c.mu.Unlock()
	}

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

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = sb.Close(ctx)
		return nil, ErrClosed
	}
	c.registry[key] = sb
	c.mu.Unlock()

	return sb, nil
}

// ListActiveSandboxes returns tracked sandboxes, pruning inactive handles.
func (c *Client) ListActiveSandboxes() []Key {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return []Key{}
	}

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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.mu.Unlock()

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
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
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
	_ = c.deleteAll(ctx)
}

func (c *Client) deleteAll(ctx context.Context) error {
	c.mu.Lock()
	snapshot := make(map[Key]*Sandbox, len(c.registry))
	maps.Copy(snapshot, c.registry)
	c.registry = make(map[Key]*Sandbox)
	c.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error

	sem := make(chan struct{}, maxCleanupConcurrency)

loop:
	for key, sb := range snapshot {
		select {
		case sem <- struct{}{}:
			delete(snapshot, key)
		case <-ctx.Done():
			c.log.Info("cleanup cancelled", "error", ctx.Err().Error())
			errMu.Lock()
			errs = append(errs, ctx.Err())
			errMu.Unlock()

			c.mu.Lock()
			if !c.closed {
				for k, v := range snapshot {
					if _, exists := c.registry[k]; !exists {
						c.registry[k] = v
					}
				}
			}
			c.mu.Unlock()
			break loop
		}

		wg.Add(1)
		go func(k Key, s *Sandbox) {
			defer func() {
				<-sem
				wg.Done()
			}()
			if err := s.Close(ctx); err != nil {
				c.log.Error(err, "cleanup failed", "claim", k.ClaimName, "namespace", k.Namespace)
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()

				c.mu.Lock()
				if !c.closed {
					if _, exists := c.registry[k]; !exists {
						c.registry[k] = s
					}
				}
				c.mu.Unlock()
			}
		}(key, sb)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// Close stops the auto-cleanup signal handler (if active) and deletes all tracked sandboxes.
// Returns an error if any of the cleanups fail (best-effort).
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	stopFn := c.cleanupStop
	c.cleanupStop = nil
	c.mu.Unlock()

	if stopFn != nil {
		stopFn()
	}

	return c.deleteAll(ctx)
}

// EnableAutoCleanup calls DeleteAll on SIGINT/SIGTERM.
// Call the returned function to stop the signal handler.
func (c *Client) EnableAutoCleanup() (stop func()) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return func() {}
	}
	if c.stopSignal != nil {
		c.mu.Unlock()
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stopSignal = cancel

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			c.mu.Lock()
			c.stopSignal = nil
			c.cleanupStop = nil
			c.mu.Unlock()
			cancel()
			signal.Stop(ch)
		})
	}
	c.cleanupStop = stopFn
	c.mu.Unlock()

	go func() {
		select {
		case sig, ok := <-ch:
			if !ok {
				return
			}
			signal.Stop(ch)
			c.mu.Lock()
			c.stopSignal = nil
			c.cleanupStop = nil
			c.mu.Unlock()
			cancel()

			c.log.Info("signal received, cleaning up sandboxes", "signal", sig.String())
			c.DeleteAll(context.Background())
			// Re-raise so the default handler terminates the process.
			p, _ := os.FindProcess(os.Getpid())
			_ = p.Signal(sig)
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return stopFn
}
