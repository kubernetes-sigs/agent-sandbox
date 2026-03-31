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

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	fakeextensions "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func newTestClient(t *testing.T) (*Client, *fakeextensions.Clientset) {
	t.Helper()
	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		APIURL:              "http://localhost:9999",
		SandboxReadyTimeout: 2 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	opts.K8sHelper = &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
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
		return true, &extv1alpha1.SandboxClaimList{
			Items: []extv1alpha1.SandboxClaim{
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

func TestClient_EnableAutoCleanup_Idempotent(t *testing.T) {
	c, _ := newTestClient(t)

	stop1 := c.EnableAutoCleanup()
	stop2 := c.EnableAutoCleanup() // should be a no-op

	stop1()
	stop2()
}

// TestResolveSandboxName_FromClaimStatus verifies the new resolution path.
func TestResolveSandboxName_FromClaimStatus(t *testing.T) {
	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	k8s := &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
		Log:              logr.Discard(),
	}

	// Seed claim with sandbox name already resolved.
	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &extv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "my-claim", Namespace: "default"},
			Status: extv1alpha1.SandboxClaimStatus{
				SandboxStatus: extv1alpha1.SandboxStatus{
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
	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	k8s := &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
		Log:              logr.Discard(),
	}

	// Seed a ready sandbox with a name different from the claim.
	agentsCS.PrependReactor("list", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &sandboxv1alpha1.SandboxList{
			Items: []sandboxv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "warm-pool-sandbox-xyz",
						Namespace: "default",
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{
							{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
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
