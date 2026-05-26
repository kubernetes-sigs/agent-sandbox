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
	"net/http"
	"strings"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace/noop"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	fakeextensions "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/typed/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func newTestClient(t *testing.T) (*Client, *fakeextensions.Clientset) {
	t.Helper()
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		APIURL:              "http://localhost:9999",
		SandboxReadyTimeout: 2 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	opts.K8sHelper = &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1beta1(),
		ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		Log:              logr.Discard(),
	}
	c, err := NewClient(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, extensionsCS
}

func TestClient_Registry(t *testing.T) {
	c, _ := newTestClient(t)

	// Empty registry.
	if got := c.ListActiveSandboxes(); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}

	// Manually inject a handle to test registry operations.
	key := Key{Namespace: "default", ClaimName: "test-claim"}
	sb := &Sandbox{log: logr.Discard()}
	sb.connector = &connector{}
	sb.connector.baseURL = "http://fake" // makes IsReady() true

	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	active := c.ListActiveSandboxes()
	if len(active) != 1 {
		t.Fatalf("expected 1 active, got %d", len(active))
	}
	if active[0].ClaimName != "test-claim" {
		t.Errorf("expected test-claim, got %s", active[0].ClaimName)
	}

	// Inactive sandboxes (baseURL=="") are pruned from the registry.
	inactive := &Sandbox{log: logr.Discard()}
	inactive.connector = &connector{} // baseURL="" -> IsReady() = false
	key = Key{Namespace: "default", ClaimName: "inactive-claim"}
	c.mu.Lock()
	c.registry[key] = inactive
	c.mu.Unlock()

	got := c.ListActiveSandboxes()
	if len(got) != 1 {
		t.Fatalf("expected 1 active after adding inactive, got %d", len(got))
	}
	c.mu.Lock()
	_, stillPresent := c.registry[key]
	c.mu.Unlock()
	if stillPresent {
		t.Error("inactive sandbox should have been pruned from registry")
	}
}

func TestClient_DeleteAll(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	// Track two fake sandboxes with claim names.
	for _, name := range []string{"claim-a", "claim-b"} {
		sb := &Sandbox{
			k8s:  c.k8s,
			log:  logr.Discard(),
			opts: c.opts,
			connector: &connector{
				strategy:   &DirectStrategy{URL: "http://fake"},
				httpClient: &http.Client{},
			},
			inflightOps:  &sync.WaitGroup{},
			lifecycleSem: make(chan struct{}, 1),
		}
		sb.connector.baseURL = "http://fake"
		sb.mu.Lock()
		sb.claimName = name
		sb.sandboxName = "sb-" + name
		sb.mu.Unlock()

		key := Key{Namespace: "default", ClaimName: name}
		c.mu.Lock()
		c.registry[key] = sb
		c.mu.Unlock()
	}

	c.DeleteAll(context.Background())

	c.mu.Lock()
	remaining := len(c.registry)
	c.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected empty registry after DeleteAll, got %d", remaining)
	}
}

func TestClient_DeleteAll_BoundedConcurrency(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	var activeDeletes atomic.Int32
	var maxActiveDeletes atomic.Int32
	var enteredCount atomic.Int32
	releaseCh := make(chan struct{})

	customExtensions := &customExtensionsClient{
		ExtensionsV1beta1Interface: extensionsCS.ExtensionsV1beta1(),
		onDelete: func(_ string) {
			active := activeDeletes.Add(1)
			for {
				currentMax := maxActiveDeletes.Load()
				if active <= currentMax {
					break
				}
				if maxActiveDeletes.CompareAndSwap(currentMax, active) {
					break
				}
			}
			if enteredCount.Add(1) == maxCleanupConcurrency {
				close(releaseCh)
			}
			select {
			case <-releaseCh:
			case <-time.After(5 * time.Second):
			}
			activeDeletes.Add(-1)
		},
	}
	c.k8s.ExtensionsClient = customExtensions

	// Setup fake reactor to succeed claim deletion immediately (inside fake client lock)
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	// Track 15 fake sandboxes (more than maxCleanupConcurrency = 10).
	const numSandboxes = 15
	for i := range numSandboxes {
		name := fmt.Sprintf("claim-%d", i)
		sb := &Sandbox{
			k8s:  c.k8s,
			log:  logr.Discard(),
			opts: c.opts,
			connector: &connector{
				strategy:   &DirectStrategy{URL: "http://fake"},
				httpClient: &http.Client{},
			},
			inflightOps:  &sync.WaitGroup{},
			lifecycleSem: make(chan struct{}, 1),
		}
		sb.connector.baseURL = "http://fake"
		sb.mu.Lock()
		sb.claimName = name
		sb.sandboxName = "sb-" + name
		sb.mu.Unlock()

		key := Key{Namespace: "default", ClaimName: name}
		c.mu.Lock()
		c.registry[key] = sb
		c.mu.Unlock()
	}

	c.DeleteAll(context.Background())

	c.mu.Lock()
	remaining := len(c.registry)
	c.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected empty registry after DeleteAll, got %d", remaining)
	}

	maxSeen := maxActiveDeletes.Load()
	if maxSeen > maxCleanupConcurrency {
		t.Errorf("concurrency limit exceeded: expected at most %d parallel close calls, got %d", maxCleanupConcurrency, maxSeen)
	}
	if maxSeen < maxCleanupConcurrency {
		// Just a sanity check that we actually reached maximum target concurrency.
		// The synchronization barrier releaseCh ensures the first 10 operations are held concurrently.
		t.Errorf("concurrency too low: expected concurrency to reach %d, got %d", maxCleanupConcurrency, maxSeen)
	}
}

func TestClient_DeleteAll_ContextCancelled_RestoresRegistry(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	// Inject a blocked delete to stall the queue
	blockCh := make(chan struct{})

	// Add 12 fake sandboxes (maxCleanupConcurrency is 10, so 2 will block)
	const numSandboxes = 12
	for i := range numSandboxes {
		name := fmt.Sprintf("claim-%d", i)
		sb := &Sandbox{
			k8s:  c.k8s,
			log:  logr.Discard(),
			opts: c.opts,
			connector: &connector{
				strategy:   &DirectStrategy{URL: "http://fake"},
				httpClient: &http.Client{},
			},
			inflightOps:  &sync.WaitGroup{},
			lifecycleSem: make(chan struct{}, 1),
		}
		sb.connector.baseURL = "http://fake"
		sb.mu.Lock()
		sb.claimName = name
		sb.sandboxName = "sb-" + name
		sb.mu.Unlock()

		key := Key{Namespace: "default", ClaimName: name}
		c.mu.Lock()
		c.registry[key] = sb
		c.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Track when exactly 10 deletions have entered the reactor
	tenDeletesActiveCh := make(chan struct{})
	var enteredCount atomic.Int32

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if enteredCount.Add(1) == maxCleanupConcurrency {
			close(tenDeletesActiveCh)
		}
		<-blockCh
		return true, nil, nil
	})

	go func() {
		// Wait until exactly 10 are active/blocked
		select {
		case <-tenDeletesActiveCh:
		case <-time.After(5 * time.Second): // safety fallback
		}
		cancel()
		close(blockCh) // unblock all to let them complete
	}()

	c.DeleteAll(ctx)

	// Verify that the 2 blocked sandboxes are restored in the registry!
	c.mu.Lock()
	remaining := len(c.registry)
	c.mu.Unlock()

	if remaining != 2 {
		t.Errorf("expected exactly 2 remaining sandboxes in registry, got %d", remaining)
	}
}

func TestClient_ListAllSandboxes(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	// Seed two claims.
	extensionsCS.PrependReactor("list", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &extv1beta1.SandboxClaimList{
			Items: []extv1beta1.SandboxClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "claim-2", Namespace: "default"}},
			},
		}, nil
	})

	names, err := c.ListAllSandboxes(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(names))
	}
}

func TestClient_DeleteSandbox_Untracked(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	deleted := false
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return true, nil, nil
	})

	if err := c.DeleteSandbox(context.Background(), "orphan-claim", "default"); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected claim deletion for untracked sandbox")
	}
}

func TestClient_CreateSandbox_EmptyTemplate(t *testing.T) {
	c, _ := newTestClient(t)

	_, err := c.CreateSandbox(context.Background(), "", "default")
	if err == nil {
		t.Error("expected error for empty template")
	}
}

func TestClient_GetSandbox_ReturnsCached(t *testing.T) {
	c, _ := newTestClient(t)

	// Inject a connected handle.
	key := Key{Namespace: "default", ClaimName: "cached-claim"}
	sb := &Sandbox{log: logr.Discard()}
	sb.connector = &connector{}
	sb.connector.baseURL = "http://fake"

	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	got, err := c.GetSandbox(context.Background(), "cached-claim", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != sb {
		t.Error("expected cached handle to be returned")
	}
}

func TestClient_DeleteSandbox_Tracked(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	sb := &Sandbox{
		k8s:  c.k8s,
		log:  logr.Discard(),
		opts: c.opts,
		connector: &connector{
			strategy:   &DirectStrategy{URL: "http://fake"},
			httpClient: &http.Client{},
		},
		inflightOps:  &sync.WaitGroup{},
		lifecycleSem: make(chan struct{}, 1),
	}
	sb.mu.Lock()
	sb.claimName = "tracked-claim"
	sb.sandboxName = "sb-tracked"
	sb.mu.Unlock()

	key := Key{Namespace: "default", ClaimName: "tracked-claim"}
	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	if err := c.DeleteSandbox(context.Background(), "tracked-claim", "default"); err != nil {
		t.Fatalf("DeleteSandbox for tracked sandbox: %v", err)
	}

	c.mu.Lock()
	remaining := len(c.registry)
	c.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected empty registry after DeleteSandbox of tracked sandbox, got %d", remaining)
	}
}

func TestClient_EnableAutoCleanup_Idempotent(t *testing.T) {
	c, _ := newTestClient(t)

	stop1 := c.EnableAutoCleanup()
	stop2 := c.EnableAutoCleanup() // should be a no-op

	stop1()
	stop2()
}

func TestClient_CleanupOnSignalOption(t *testing.T) {
	// 1. With CleanupOnSignal false (default)
	c1, _ := newTestClient(t)
	c1.mu.Lock()
	stopSignalNil := c1.stopSignal == nil
	cleanupStopNil := c1.cleanupStop == nil
	c1.mu.Unlock()

	if !stopSignalNil {
		t.Error("expected stopSignal to be nil by default")
	}
	if !cleanupStopNil {
		t.Error("expected cleanupStop to be nil by default")
	}

	// 2. With CleanupOnSignal true
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck
	opts := Options{
		TemplateName:    "test-template",
		Namespace:       "default",
		APIURL:          "http://localhost:9999",
		Quiet:           true,
		CleanupOnSignal: true,
	}
	opts.setDefaults()
	opts.K8sHelper = &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1beta1(),
		ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		Log:              logr.Discard(),
	}
	c2, err := NewClient(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	c2.mu.Lock()
	stopSignalActive := c2.stopSignal != nil
	cleanupStopActive := c2.cleanupStop != nil
	c2.mu.Unlock()

	if !stopSignalActive {
		t.Error("expected stopSignal to be active when CleanupOnSignal is true")
	}
	if !cleanupStopActive {
		t.Error("expected cleanupStop to be active when CleanupOnSignal is true")
	}

	// Clean up resources / stop signal handler for test environment
	if cleanupStopActive {
		c2.mu.Lock()
		stopFn := c2.cleanupStop
		c2.mu.Unlock()
		stopFn()
	}
}

// TestResolveSandboxName_FromClaimStatus verifies the new resolution path.
func TestResolveSandboxName_FromClaimStatus(t *testing.T) {
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	k8s := &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1beta1(),
		ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		Log:              logr.Discard(),
	}

	// Seed claim with sandbox name already resolved.
	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &extv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "my-claim", Namespace: "default"},
			Status: extv1beta1.SandboxClaimStatus{
				SandboxStatus: extv1beta1.SandboxStatus{
					Name: "warm-pool-sandbox-xyz",
				},
			},
		}, nil
	})

	name, err := k8s.resolveSandboxName(context.Background(), "my-claim", "default", 5*time.Second, noop.NewTracerProvider().Tracer("test"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if name != "warm-pool-sandbox-xyz" {
		t.Errorf("expected warm-pool-sandbox-xyz, got %s", name)
	}
}

// TestWaitForSandboxReady_UsesSandboxName verifies the ready check uses the
// resolved sandbox name, not the claim name.
func TestWaitForSandboxReady_UsesSandboxName(t *testing.T) {
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	k8s := &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1beta1(),
		ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		Log:              logr.Discard(),
	}

	// Seed a ready sandbox with a name different from the claim.
	agentsCS.PrependReactor("list", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &sandboxv1beta1.SandboxList{
			Items: []sandboxv1beta1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warm-pool-sandbox-xyz",
						Namespace: "default",
					},
					Status: sandboxv1beta1.SandboxStatus{
						Conditions: []metav1.Condition{
							{Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue},
						},
					},
				},
			},
		}, nil
	})

	state, err := k8s.waitForSandboxReady(context.Background(), "warm-pool-sandbox-xyz", "default", 5*time.Second, noop.NewTracerProvider().Tracer("test"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.SandboxName != "warm-pool-sandbox-xyz" {
		t.Errorf("expected warm-pool-sandbox-xyz, got %s", state.SandboxName)
	}
}

func TestClient_Close(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	// Inject a custom cleanupStop hook to track if Close() stops it
	stopCalled := false
	c.mu.Lock()
	c.cleanupStop = func() {
		stopCalled = true
	}
	c.mu.Unlock()

	// Track a dummy sandbox in registry to verify registry deletion on Close()
	sb := &Sandbox{
		k8s:  c.k8s,
		log:  logr.Discard(),
		opts: c.opts,
		connector: &connector{
			strategy:   &DirectStrategy{URL: "http://fake"},
			httpClient: &http.Client{},
		},
		inflightOps:  &sync.WaitGroup{},
		lifecycleSem: make(chan struct{}, 1),
	}
	sb.connector.baseURL = "http://fake"
	sb.mu.Lock()
	sb.claimName = "tracked-claim-close"
	sb.sandboxName = "sb-tracked-close"
	sb.mu.Unlock()

	key := Key{Namespace: "default", ClaimName: "tracked-claim-close"}
	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	// Call Close
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify stop was called
	if !stopCalled {
		t.Error("expected cleanupStop to be called during Close")
	}

	// Verify cleanupStop is now nil
	c.mu.Lock()
	cleanupStopNil := c.cleanupStop == nil
	remaining := len(c.registry)
	c.mu.Unlock()

	if !cleanupStopNil {
		t.Error("expected cleanupStop to be nil after Close")
	}
	if remaining != 0 {
		t.Errorf("expected empty registry after Close, got %d", remaining)
	}
}

func TestClient_ClosedClientErrors(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("unexpected error closing client: %v", err)
	}

	// 1. CreateSandbox returns ErrClosed
	_, err := c.CreateSandbox(context.Background(), "template", "default")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}

	// 2. GetSandbox returns ErrClosed
	_, err = c.GetSandbox(context.Background(), "claim", "default")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}

	// 3. DeleteSandbox returns ErrClosed
	err = c.DeleteSandbox(context.Background(), "claim", "default")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}

	// 4. ListActiveSandboxes returns nil
	if active := c.ListActiveSandboxes(); active != nil {
		t.Errorf("expected nil active list, got %v", active)
	}

	// 5. ListAllSandboxes returns ErrClosed
	_, err = c.ListAllSandboxes(context.Background(), "default")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}
}

func TestClient_Close_Idempotent(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second close failed (expected no error): %v", err)
	}
}

func TestClient_Close_ErrorAggregation(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	// Inject two fake sandboxes: one succeeds and one fails to delete
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(ktesting.DeleteAction)
		if deleteAction.GetName() == "fail-claim" {
			return true, nil, fmt.Errorf("injected deletion error")
		}
		return true, nil, nil
	})

	for _, name := range []string{"success-claim", "fail-claim"} {
		sb := &Sandbox{
			k8s:  c.k8s,
			log:  logr.Discard(),
			opts: c.opts,
			connector: &connector{
				strategy:   &DirectStrategy{URL: "http://fake"},
				httpClient: &http.Client{},
			},
			inflightOps:  &sync.WaitGroup{},
			lifecycleSem: make(chan struct{}, 1),
		}
		sb.connector.baseURL = "http://fake"
		sb.mu.Lock()
		sb.claimName = name
		sb.sandboxName = "sb-" + name
		sb.mu.Unlock()

		key := Key{Namespace: "default", ClaimName: name}
		c.mu.Lock()
		c.registry[key] = sb
		c.mu.Unlock()
	}

	err := c.Close(context.Background())
	if err == nil {
		t.Error("expected Close to return aggregated errors, got nil")
	} else if !strings.Contains(err.Error(), "injected deletion error") {
		t.Errorf("expected error to contain target failure details, got: %v", err)
	}
}

func TestClient_DeleteAll_RestoresFailedRegistry(t *testing.T) {
	c, extensionsCS := newTestClient(t)

	// Inject a deletion failure
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected deletion error")
	})

	sb := &Sandbox{
		k8s:  c.k8s,
		log:  logr.Discard(),
		opts: c.opts,
		connector: &connector{
			strategy:   &DirectStrategy{URL: "http://fake"},
			httpClient: &http.Client{},
		},
		inflightOps:  &sync.WaitGroup{},
		lifecycleSem: make(chan struct{}, 1),
	}
	sb.connector.baseURL = "http://fake"
	sb.mu.Lock()
	sb.claimName = "fail-claim"
	sb.sandboxName = "sb-fail"
	sb.mu.Unlock()

	key := Key{Namespace: "default", ClaimName: "fail-claim"}
	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	// Call DeleteAll
	c.DeleteAll(context.Background())

	// Verify that the failed sandbox was restored in the registry!
	c.mu.Lock()
	remaining := len(c.registry)
	restoredSb := c.registry[key]
	c.mu.Unlock()

	if remaining != 1 {
		t.Errorf("expected exactly 1 remaining sandbox in registry, got %d", remaining)
	}
	if restoredSb != sb {
		t.Error("expected the exact same sandbox handle to be restored")
	}
}

type customSandboxClaims struct {
	extensionsv1beta1.SandboxClaimInterface
	onDelete func(name string)
}

func (c *customSandboxClaims) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	if c.onDelete != nil {
		c.onDelete(name)
	}
	return c.SandboxClaimInterface.Delete(ctx, name, opts)
}

type customExtensionsClient struct {
	extensionsv1beta1.ExtensionsV1beta1Interface
	onDelete func(name string)
}

func (c *customExtensionsClient) SandboxClaims(namespace string) extensionsv1beta1.SandboxClaimInterface {
	realClaims := c.ExtensionsV1beta1Interface.SandboxClaims(namespace)
	return &customSandboxClaims{
		SandboxClaimInterface: realClaims,
		onDelete:              c.onDelete,
	}
}
