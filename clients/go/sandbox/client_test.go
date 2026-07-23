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
	"net/http"
	"sync"
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
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func newTestClient(t *testing.T) (*Client, *fakeextensions.Clientset) {
	t.Helper()
	return newTestClientWithOptions(t, nil)
}

func newTestClientWithOptions(t *testing.T, customize func(*Options)) (*Client, *fakeextensions.Clientset) {
	t.Helper()
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	opts := Options{
		WarmPoolName:        "test-warmpool",
		Namespace:           "default",
		APIURL:              "http://localhost:9999",
		SandboxReadyTimeout: 2 * time.Second,
		Quiet:               true,
	}
	if customize != nil {
		customize(&opts)
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

func TestClient_CreateSandbox_EmptyWarmPool(t *testing.T) {
	c, _ := newTestClient(t)

	_, err := c.CreateSandbox(context.Background(), "", "default")
	if err == nil {
		t.Error("expected error for empty warm pool")
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

// TestClient_CreateSandbox_RedundantHandleKeepsSharedClaim guards the registry
// re-check branch in CreateSandbox. The registry key is the server-assigned
// (GenerateName) claim name, so a hit under our key means a concurrent
// GetSandbox already attached to the very claim we just created and registered a
// ready handle first. In that case CreateSandbox must tear down only its
// redundant transport (Disconnect) and NOT delete the shared claim — otherwise
// it deletes the sandbox out from under the tracked handle it returns.
func TestClient_CreateSandbox_RedundantHandleKeepsSharedClaim(t *testing.T) {
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig

	opts := Options{
		WarmPoolName:        "test-warmpool",
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

	// Resolve the sandbox name from the claim status (claim name == sandbox name).
	extensionsCS.PrependReactor("get", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		ga := action.(ktesting.GetAction)
		return true, &extv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()},
			Status: extv1beta1.SandboxClaimStatus{
				SandboxStatus: extv1beta1.SandboxStatus{Name: ga.GetName()},
			},
		}, nil
	})

	// Wire create (GenerateName -> deterministic name) and a ready sandbox on
	// list/watch so CreateSandbox's Open() succeeds.
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("placeholder"))

	// Fail loudly if the shared claim is ever deleted: the old Close()-based
	// path in this branch would issue a delete here.
	var deleteCalled bool
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		deleteCalled = true
		return true, nil, nil
	})

	c, err := NewClient(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	// setupWatchWithReactor assigns "<GenerateName>test12345" to created claims,
	// so the key CreateSandbox computes is known ahead of time. Pre-seed a ready
	// handle under it to force the re-check branch to fire deterministically.
	const claimName = "sandbox-claim-test12345"
	tracked := &Sandbox{log: logr.Discard()}
	tracked.connector = &connector{}
	tracked.connector.baseURL = "http://fake" // makes IsReady() true
	key := Key{Namespace: "default", ClaimName: claimName}
	c.mu.Lock()
	c.registry[key] = tracked
	c.mu.Unlock()

	got, err := c.CreateSandbox(context.Background(), "test-warmpool", "default")
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	if got != tracked {
		t.Error("expected the already-tracked handle to be returned, not the redundant one")
	}
	if deleteCalled {
		t.Error("shared claim was deleted; redundant handle must be Disconnect()ed, not Close()d")
	}
	if !tracked.IsReady() {
		t.Error("tracked handle should remain ready after the redundant handle is torn down")
	}

	// The registry must still hold the tracked handle under the shared key.
	c.mu.Lock()
	stillTracked := c.registry[key]
	c.mu.Unlock()
	if stillTracked != tracked {
		t.Error("expected tracked handle to remain registered under the shared key")
	}
}

// TestClient_GetSandbox_AdoptsRacingHandleKeepsSharedClaim guards the registry
// re-check branch in GetSandbox — the symmetric counterpart to the CreateSandbox
// branch. While GetSandbox is resolving+attaching, a concurrent
// GetSandbox/CreateSandbox for the same key can install a ready handle first. In
// that case GetSandbox must return the already-tracked handle and tear down only
// its own redundant transport (Disconnect), never deleting the shared claim.
//
// The race is forced deterministically: a "get sandboxclaims" reactor installs a
// ready tracked handle during resolveSandboxName (before this GetSandbox finishes
// attaching), so by the time it reaches trackOrAdoptRace the adopt branch is
// guaranteed to fire.
func TestClient_GetSandbox_AdoptsRacingHandleKeepsSharedClaim(t *testing.T) {
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig

	opts := Options{
		WarmPoolName:        "test-warmpool",
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

	const claimName = "shared-claim"
	key := Key{Namespace: "default", ClaimName: claimName}

	// The handle a concurrent caller registers first. GetSandbox must return
	// exactly this handle and leave it registered.
	tracked := &Sandbox{log: logr.Discard()}
	tracked.connector = &connector{}
	tracked.connector.baseURL = "http://fake" // makes IsReady() true

	// Reactor for every "get sandboxclaims" (verifyClaimExists +
	// resolveSandboxName). On the resolveSandboxName call — the second get — it
	// installs the ready tracked handle under key, simulating a concurrent caller
	// that wins the race while this GetSandbox is still attaching.
	var getCalls int
	extensionsCS.PrependReactor("get", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 2 {
			c.mu.Lock()
			c.registry[key] = tracked
			c.mu.Unlock()
		}
		ga := action.(ktesting.GetAction)
		return true, &extv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()},
			Status: extv1beta1.SandboxClaimStatus{
				SandboxStatus: extv1beta1.SandboxStatus{Name: ga.GetName()},
			},
		}, nil
	})

	// Reconnect path: verifySandboxAlive gets a ready sandbox so the redundant
	// handle this GetSandbox builds actually opens — proving adoption happens
	// because a ready handle already exists, not because our own attach failed.
	agentsCS.PrependReactor("get", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		ga := action.(ktesting.GetAction)
		return true, readySandbox(ga.GetName()), nil
	})

	// Fail loudly if the shared claim is ever deleted: the buggy Close()-based
	// path in this branch would issue a delete here.
	var deleteCalled bool
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		deleteCalled = true
		return true, nil, nil
	})

	got, err := c.GetSandbox(context.Background(), claimName, "default")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}

	if got != tracked {
		t.Error("expected the already-tracked handle to be returned, not the redundant one")
	}
	if deleteCalled {
		t.Error("shared claim was deleted; redundant handle must be Disconnect()ed, not Close()d")
	}
	if !tracked.IsReady() {
		t.Error("tracked handle should remain ready after the redundant handle is torn down")
	}

	// The registry must still hold the tracked handle under the shared key.
	c.mu.Lock()
	stillTracked := c.registry[key]
	c.mu.Unlock()
	if stillTracked != tracked {
		t.Error("expected tracked handle to remain registered under the shared key")
	}
}

func TestClient_EnableAutoCleanup_Idempotent(t *testing.T) {
	c, _ := newTestClient(t)

	stop1 := c.EnableAutoCleanup()
	stop2 := c.EnableAutoCleanup() // should be a no-op

	stop1()
	stop2()
}

func TestClient_EnableAutoCleanup_FromOptions_Idempotent(t *testing.T) {
	client, _ := newTestClientWithOptions(t, func(opts *Options) {
		opts.CleanupOnSignal = true
	})

	// Manually call EnableAutoCleanup again (should be safe/idempotent)
	_ = client.EnableAutoCleanup()

	// Verify cleanup is still enabled (not broken by double-enable)
	client.mu.Lock()
	enabled := client.cleanupEnabled
	client.mu.Unlock()

	if !enabled {
		t.Error("cleanup should still be enabled after calling EnableAutoCleanup twice")
	}

	// Clean up signal handlers
	client.DisableAutoCleanup()
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

func TestClient_CleanupOnSignal_Default(t *testing.T) {
	// Verify CleanupOnSignal defaults to false
	var opts Options
	if opts.CleanupOnSignal {
		t.Error("CleanupOnSignal should default to false")
	}

	// Create client with default options
	client, _ := newTestClient(t)

	// Verify cleanup is not enabled
	client.mu.Lock()
	enabled := client.cleanupEnabled
	client.mu.Unlock()

	if enabled {
		t.Error("cleanup should not be enabled by default")
	}
}

func TestClient_CleanupOnSignal_Enabled(t *testing.T) {
	client, _ := newTestClientWithOptions(t, func(opts *Options) {
		opts.CleanupOnSignal = true
	})

	// Verify cleanup is enabled
	client.mu.Lock()
	enabled := client.cleanupEnabled
	hasStopSignal := client.stopSignal != nil
	client.mu.Unlock()

	if !enabled {
		t.Error("cleanup should be enabled when CleanupOnSignal is true")
	}

	if !hasStopSignal {
		t.Error("signal handler should be registered when CleanupOnSignal is true")
	}

	// Clean up signal handler
	client.DisableAutoCleanup()
}

func TestClient_MultipleClients_Independent(t *testing.T) {
	// Client 1 with cleanup enabled
	client1, _ := newTestClientWithOptions(t, func(opts *Options) {
		opts.CleanupOnSignal = true
	})

	// Client 2 with cleanup disabled
	client2, _ := newTestClient(t)

	// Verify client 1 has cleanup enabled
	client1.mu.Lock()
	enabled1 := client1.cleanupEnabled
	client1.mu.Unlock()

	if !enabled1 {
		t.Error("client 1 should have cleanup enabled")
	}

	// Verify client 2 does not have cleanup enabled
	client2.mu.Lock()
	enabled2 := client2.cleanupEnabled
	client2.mu.Unlock()

	if enabled2 {
		t.Error("client 2 should not have cleanup enabled")
	}

	// Clean up signal handlers
	client1.DisableAutoCleanup()
	client2.DisableAutoCleanup()
}
