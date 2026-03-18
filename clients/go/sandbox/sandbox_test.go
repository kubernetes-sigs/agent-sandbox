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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	fakeextensions "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Shared test helpers
// ---------------------------------------------------------------------------

func defaultTestOpts() Options {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		APIURL:              "http://localhost:9999",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	return opts
}

func newTestSandbox(opts Options) (*Sandbox, *fakeagents.Clientset, *fakeextensions.Clientset) {
	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	opts.K8sHelper = &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
		Log:              opts.Logger,
	}
	s, err := New(context.Background(), opts)
	if err != nil {
		panic("newTestSandbox: " + err.Error())
	}
	return s, agentsCS, extensionsCS
}

func newTestSandboxWithDynamic(opts Options, dynCS *fakedynamic.FakeDynamicClient) (*Sandbox, *fakeagents.Clientset, *fakeextensions.Clientset) {
	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	opts.K8sHelper = &K8sHelper{
		AgentsClient:     agentsCS.AgentsV1alpha1(),
		ExtensionsClient: extensionsCS.ExtensionsV1alpha1(),
		DynamicClient:    dynCS,
		Log:              opts.Logger,
	}
	s, err := New(context.Background(), opts)
	if err != nil {
		panic("newTestSandboxWithDynamic: " + err.Error())
	}
	return s, agentsCS, extensionsCS
}

func readySandbox(name string) *sandboxv1alpha1.Sandbox {
	return &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				PodNameAnnotation: name + "-pod",
			},
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{
				{
					Type:   string(sandboxv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
}

// setupWatchWithReactor wires a fake watcher that sends the sandbox once
// the claim is created, eliminating time.Sleep-based coordination.
func setupWatchWithReactor(agentsCS *fakeagents.Clientset, extensionsCS *fakeextensions.Clientset, sb *sandboxv1alpha1.Sandbox) {
	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				matched := sb.DeepCopy()
				matched.Name = claim.Name
				if matched.Annotations != nil {
					if _, has := matched.Annotations[PodNameAnnotation]; has {
						matched.Annotations[PodNameAnnotation] = claim.Name + "-pod"
					}
				}
				go fakeWatcher.Add(matched)
				return false, nil, nil
			}
		}
		go fakeWatcher.Add(sb)
		return false, nil, nil
	})
}

// ---------------------------------------------------------------------------
// Validation tests
// ---------------------------------------------------------------------------

func TestNew_MissingTemplateName(t *testing.T) {
	opts := Options{Namespace: "default"}
	opts.setDefaults()
	if err := opts.validate(); err == nil {
		t.Fatal("expected error for missing TemplateName")
	}
}

func TestNew_InvalidPort(t *testing.T) {
	opts := Options{TemplateName: "tpl", ServerPort: -1}
	opts.setDefaults()
	if err := opts.validate(); err == nil {
		t.Fatal("expected error for negative ServerPort")
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	opts := Options{TemplateName: "tpl"}
	opts.setDefaults()
	if opts.Namespace != "default" {
		t.Errorf("expected Namespace=default, got %s", opts.Namespace)
	}
	if opts.ServerPort != 8888 {
		t.Errorf("expected ServerPort=8888, got %d", opts.ServerPort)
	}
	if opts.SandboxReadyTimeout != 180*time.Second {
		t.Errorf("expected SandboxReadyTimeout=180s, got %s", opts.SandboxReadyTimeout)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle tests
// ---------------------------------------------------------------------------

func TestOpen_CreatesClaimAndBecomesReady(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("test-sandbox"))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() returned error: %v", err)
	}
	defer c.Close(context.Background())

	if c.ClaimName() == "" {
		t.Error("expected non-empty claim name after Open")
	}
	if !c.IsReady() {
		t.Error("expected IsReady()=true after Open")
	}
	expectedPod := c.ClaimName() + "-pod"
	if c.PodName() != expectedPod {
		t.Errorf("expected PodName=%s, got %s", expectedPod, c.PodName())
	}
	if c.SandboxName() != c.ClaimName() {
		t.Errorf("expected SandboxName to match ClaimName %s, got %s", c.ClaimName(), c.SandboxName())
	}
	if c.connector.BaseURL() != opts.APIURL {
		t.Errorf("expected baseURL=%s, got %s", opts.APIURL, c.connector.BaseURL())
	}
}

func TestOpen_ListBeforeWatch_AlreadyReady(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, _ := newTestSandbox(opts)

	// The list reactor returns a sandbox whose name matches the claim name
	// extracted from the field selector. This validates the M1 name check.
	agentsCS.PrependReactor("list", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		la := action.(ktesting.ListAction)
		selector := la.GetListRestrictions().Fields.String()
		if selector == "" || !strings.Contains(selector, "metadata.name=") {
			t.Errorf("expected field selector with metadata.name, got %q", selector)
		}
		// Extract claim name from field selector "metadata.name=<claimName>".
		claimName := strings.TrimPrefix(selector, "metadata.name=")
		sb := readySandbox(claimName)
		return true, &sandboxv1alpha1.SandboxList{Items: []sandboxv1alpha1.Sandbox{*sb}}, nil
	})

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() returned error: %v", err)
	}
	defer c.Close(context.Background())

	if c.SandboxName() == "" {
		t.Error("expected non-empty SandboxName from List path")
	}
	if c.SandboxName() != c.ClaimName() {
		t.Errorf("expected SandboxName=%s to match ClaimName=%s", c.SandboxName(), c.ClaimName())
	}
}

func TestOpen_TimeoutWhenNotReady(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 200 * time.Millisecond
	c, agentsCS, _ := newTestSandbox(opts)

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	go func() {
		fakeWatcher.Add(&sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse},
				},
			},
		})
	}()

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected timeout error from Open")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got: %v", err)
	}
}

func TestClose_DeletesClaim(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() returned error: %v", err)
	}

	claimName := c.ClaimName()
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}

	if c.IsReady() {
		t.Error("expected IsReady()=false after Close")
	}

	found := false
	for _, action := range extensionsCS.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "sandboxclaims" {
			da, ok := action.(ktesting.DeleteAction)
			if ok && da.GetName() == claimName {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected a delete action for the SandboxClaim")
	}
}

func TestClose_ToleratesAlreadyClosed(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close() on unopened client returned error: %v", err)
	}
}

func TestOpen_PodNameFallsBackToSandboxName(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb-no-annotation",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{
				{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
		},
	}
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() returned error: %v", err)
	}
	defer c.Close(context.Background())

	if c.PodName() != c.SandboxName() {
		t.Errorf("expected PodName to fall back to sandbox name %s, got %s", c.SandboxName(), c.PodName())
	}
}

// ---------------------------------------------------------------------------
// Mode selection tests
// ---------------------------------------------------------------------------

func TestModeSelection_DirectURL(t *testing.T) {
	opts := defaultTestOpts()
	opts.APIURL = "http://direct.example.com"
	opts.GatewayName = ""
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if c.connector.BaseURL() != "http://direct.example.com" {
		t.Errorf("expected direct URL mode, got baseURL=%s", c.connector.BaseURL())
	}
}

func TestModeSelection_APIURLTakesPrecedence(t *testing.T) {
	opts := defaultTestOpts()
	opts.APIURL = "http://direct.example.com"
	opts.GatewayName = "should-be-ignored"
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if c.connector.BaseURL() != "http://direct.example.com" {
		t.Errorf("expected APIURL to take precedence, got baseURL=%s", c.connector.BaseURL())
	}
}

// ---------------------------------------------------------------------------
// IsReady does not block during Open
// ---------------------------------------------------------------------------

func TestIsReady_DoesNotBlockDuringOpen(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 500 * time.Millisecond
	c, agentsCS, _ := newTestSandbox(opts)

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	done := make(chan struct{})
	go func() {
		_ = c.Open(context.Background())
		close(done)
	}()

	ready := make(chan bool, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		ready <- c.IsReady()
	}()

	select {
	case r := <-ready:
		if r {
			t.Error("expected IsReady()=false during Open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IsReady() blocked — c.mu is still held across blocking I/O")
	}

	<-done
}

// ---------------------------------------------------------------------------
// resolveRouterPod tests (TunnelStrategy)
// ---------------------------------------------------------------------------

func TestResolveRouterPod_Success(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-router-svc-abc",
			Namespace: "default",
			Labels:    map[string]string{"kubernetes.io/service-name": "sandbox-router-svc"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Name: "router-pod-abc"},
		}},
	}

	kubeCS := fakekube.NewSimpleClientset(slice)
	ts := &tunnelStrategy{
		discoveryClient: kubeCS.DiscoveryV1(),
		namespace:       "default",
	}

	pod, err := ts.resolveRouterPod(context.Background())
	if err != nil {
		t.Fatalf("resolveRouterPod() error: %v", err)
	}
	if pod != "router-pod-abc" {
		t.Errorf("expected router-pod-abc, got %s", pod)
	}
}

func TestResolveRouterPod_NoEndpoints(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-router-svc-abc",
			Namespace: "default",
			Labels:    map[string]string{"kubernetes.io/service-name": "sandbox-router-svc"},
		},
	}

	kubeCS := fakekube.NewSimpleClientset(slice)
	ts := &tunnelStrategy{
		discoveryClient: kubeCS.DiscoveryV1(),
		namespace:       "default",
	}

	_, err := ts.resolveRouterPod(context.Background())
	if err == nil {
		t.Fatal("expected error for empty endpoints")
	}
}

// ---------------------------------------------------------------------------
// Gateway discovery tests
// ---------------------------------------------------------------------------

func TestModeSelection_Gateway(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	dynCS.PrependWatchReactor("gateways", func(_ ktesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFake()
		go fw.Add(&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "gateway.networking.k8s.io/v1",
				"kind":       "Gateway",
				"metadata":   map[string]interface{}{"name": "test-gateway", "namespace": "default"},
				"status": map[string]interface{}{
					"addresses": []interface{}{
						map[string]interface{}{"type": "IPAddress", "value": "203.0.113.10"},
					},
				},
			},
		})
		return true, fw, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() with gateway mode error: %v", err)
	}
	defer c.Close(context.Background())

	if c.connector.BaseURL() != "http://203.0.113.10" {
		t.Errorf("expected gateway IP in baseURL, got %s", c.connector.BaseURL())
	}
}

func TestGatewayDiscovery_Timeout(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "missing-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 200 * time.Millisecond,
		Quiet:               true,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	gwWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("gateways", ktesting.DefaultWatchReactor(gwWatcher, nil))

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected timeout error for gateway discovery")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got: %v", err)
	}
}

func TestExtractGatewayAddress(t *testing.T) {
	cases := []struct {
		name     string
		obj      map[string]interface{}
		expected string
	}{
		{"ip address", map[string]interface{}{
			"status": map[string]interface{}{
				"addresses": []interface{}{map[string]interface{}{"value": "10.0.0.1"}},
			},
		}, "10.0.0.1"},
		{"hostname", map[string]interface{}{
			"status": map[string]interface{}{
				"addresses": []interface{}{map[string]interface{}{"value": "gateway.example.com"}},
			},
		}, "gateway.example.com"},
		{"empty value", map[string]interface{}{
			"status": map[string]interface{}{
				"addresses": []interface{}{map[string]interface{}{"value": ""}},
			},
		}, ""},
		{"no status", map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &unstructured.Unstructured{Object: tc.obj}
			if got, _ := extractGatewayAddress(gw); got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestGatewayDiscovery_WatchUsesCorrectFieldSelector(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	var capturedSelector string
	dynCS.PrependWatchReactor("gateways", func(action ktesting.Action) (bool, watch.Interface, error) {
		wa := action.(ktesting.WatchAction)
		capturedSelector = wa.GetWatchRestrictions().Fields.String()
		fw := watch.NewFake()
		go fw.Add(&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "gateway.networking.k8s.io/v1",
				"kind":       "Gateway",
				"metadata":   map[string]interface{}{"name": "test-gateway", "namespace": "default"},
				"status": map[string]interface{}{
					"addresses": []interface{}{map[string]interface{}{"value": "203.0.113.10"}},
				},
			},
		})
		return true, fw, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if capturedSelector == "" {
		t.Fatal("expected gateway watch to have a field selector")
	}
	if !strings.Contains(capturedSelector, "metadata.name=test-gateway") {
		t.Errorf("expected field selector 'metadata.name=test-gateway', got %q", capturedSelector)
	}
}

func TestGatewayDiscovery_ListUsesCorrectFieldSelector(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	var capturedSelector string
	dynCS.PrependReactor("list", "gateways", func(action ktesting.Action) (bool, runtime.Object, error) {
		la := action.(ktesting.ListAction)
		capturedSelector = la.GetListRestrictions().Fields.String()
		return true, &unstructured.UnstructuredList{
			Object: map[string]interface{}{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayList"},
			Items: []unstructured.Unstructured{{
				Object: map[string]interface{}{
					"apiVersion": "gateway.networking.k8s.io/v1",
					"kind":       "Gateway",
					"metadata":   map[string]interface{}{"name": "test-gateway", "namespace": "default"},
					"status":     map[string]interface{}{"addresses": []interface{}{map[string]interface{}{"value": "198.51.100.1"}}},
				},
			}},
		}, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if capturedSelector == "" {
		t.Fatal("expected gateway list to have a field selector")
	}
	if !strings.Contains(capturedSelector, "metadata.name=test-gateway") {
		t.Errorf("expected field selector 'metadata.name=test-gateway', got %q", capturedSelector)
	}
}

func TestGatewayGVR(t *testing.T) {
	expected := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
	if gatewayGVR != expected {
		t.Errorf("gatewayGVR mismatch: got %v, want %v", gatewayGVR, expected)
	}
}

func TestExtractGatewayAddress_SSRFRejection(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"path separator", "evil.com/admin"},
		{"query string", "evil.com?redirect=1"},
		{"fragment", "evil.com#frag"},
		{"userinfo", "user@evil.com"},
		{"combined ssrf", "user@evil.com/admin?x=1#f"},
		{"invalid hostname: leading hyphen", "-bad.example.com"},
		{"invalid hostname: trailing dot", "host.example."},
		{"invalid hostname: double dot", "host..example.com"},
		{"invalid hostname: underscore", "host_name.com"},
		{"invalid hostname: space", "host name.com"},
		{"invalid hostname: colon", "host:8080"},
		{"invalid hostname: trailing hyphen", "host-.example.com"},
		{"invalid hostname: label starts with hyphen", "host.-bad.com"},
		{"invalid hostname: empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &unstructured.Unstructured{Object: map[string]interface{}{
				"status": map[string]interface{}{
					"addresses": []interface{}{map[string]interface{}{"value": tc.value}},
				},
			}}
			if got, _ := extractGatewayAddress(gw); got != "" {
				t.Errorf("expected empty (rejected), got %q", got)
			}
		})
	}
}

func TestIsValidGatewayHostname(t *testing.T) {
	valid := []string{
		"example.com",
		"a.b.c.example.com",
		"gateway-1.example.com",
		"a",
		"A.B.C",
		"123.example.com",
	}
	for _, h := range valid {
		if !isValidGatewayHostname(h) {
			t.Errorf("expected valid: %q", h)
		}
	}

	invalid := []string{
		"",
		"-leading.com",
		"trailing-.com",
		"double..dot.com",
		".leading.dot",
		"trailing.dot.",
		"under_score.com",
		"spa ce.com",
		"col:on.com",
		"slash/path.com",
		"quer?y.com",
		"hash#.com",
		"at@sign.com",
	}
	for _, h := range invalid {
		if isValidGatewayHostname(h) {
			t.Errorf("expected invalid: %q", h)
		}
	}

	// 253-char hostname should be valid; 254-char should not.
	long253 := strings.Repeat("a", 253)
	if !isValidGatewayHostname(long253) {
		t.Error("expected 253-char hostname to be valid")
	}
	long254 := strings.Repeat("a", 254)
	if isValidGatewayHostname(long254) {
		t.Error("expected 254-char hostname to be invalid")
	}
}

// ---------------------------------------------------------------------------
// Rollback and edge-case tests
// ---------------------------------------------------------------------------

func TestOpen_RollbackDeletesClaim(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 200 * time.Millisecond
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected timeout error from Open")
	}

	if c.ClaimName() != "" {
		t.Errorf("expected empty ClaimName after rollback, got %q", c.ClaimName())
	}

	found := false
	for _, action := range extensionsCS.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "sandboxclaims" {
			found = true
		}
	}
	if !found {
		t.Error("expected a delete action for the SandboxClaim during rollback")
	}
}

func TestClose_DeleteClaim_NotFound(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	claimName := c.ClaimName()
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewNotFound(
			schema.GroupResource{Group: "extensions.agents.x-k8s.io", Resource: "sandboxclaims"},
			claimName,
		)
	})

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close() should succeed on NotFound, got error: %v", err)
	}
	if c.ClaimName() != "" {
		t.Error("expected empty ClaimName after Close with NotFound")
	}
}

func TestFormatURL_IPv6(t *testing.T) {
	g := &gatewayStrategy{gatewayScheme: "http"}

	if got := g.formatURL("2001:db8::1"); got != "http://[2001:db8::1]" {
		t.Errorf("expected http://[2001:db8::1], got %s", got)
	}

	if got := g.formatURL("10.0.0.1"); got != "http://10.0.0.1" {
		t.Errorf("expected http://10.0.0.1, got %s", got)
	}
}

func TestOpen_CloseReopens(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	var mu sync.Mutex
	currentWatcher := watch.NewFake()

	agentsCS.PrependWatchReactor("sandboxes", func(_ ktesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		w := currentWatcher
		mu.Unlock()
		return true, w, nil
	})
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := "sb"
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				name = claim.Name
			}
		}
		mu.Lock()
		w := currentWatcher
		mu.Unlock()
		go w.Add(readySandbox(name))
		return false, nil, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("first Open() error: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if c.IsReady() {
		t.Error("expected IsReady()=false after Close")
	}

	mu.Lock()
	currentWatcher = watch.NewFake()
	mu.Unlock()

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("second Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if !c.IsReady() {
		t.Error("expected IsReady()=true after second Open")
	}
}

// ---------------------------------------------------------------------------
// ErrAlreadyOpen tests
// ---------------------------------------------------------------------------

func TestOpen_DoubleOpen_ReturnsErrAlreadyOpen(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("first Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if err := c.Open(context.Background()); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("expected ErrAlreadyOpen on second Open(), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// createClaim failure
// ---------------------------------------------------------------------------

func TestOpen_CreateClaimFailure(t *testing.T) {
	opts := defaultTestOpts()
	c, _, extensionsCS := newTestSandbox(opts)

	extensionsCS.PrependReactor("create", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden: insufficient permissions")
	})

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error from createClaim failure")
	}
	if !errors.Is(err, ErrClaimFailed) {
		t.Errorf("expected ErrClaimFailed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// drainSandboxWatch tests (K8sHelper method)
// ---------------------------------------------------------------------------

func TestDrainSandboxWatch_ErrorTriggersRelist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status metav1.Status
	}{
		{"410Gone", metav1.Status{Code: 410, Reason: metav1.StatusReasonGone}},
		{"500Internal", metav1.Status{Code: 500, Message: "internal error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &K8sHelper{Log: logr.Discard()}
			fw := watch.NewFake()
			go func() { fw.Error(&tc.status) }()

			var lastCond string
			_, done, err := h.drainSandboxWatch(context.Background(), fw, "test-claim", 5*time.Second, &lastCond)
			if done {
				t.Error("expected done=false")
			}
			if err != nil {
				t.Errorf("expected nil error (should trigger re-list), got: %v", err)
			}
		})
	}
}

func TestDrainSandboxWatch_Deleted(t *testing.T) {
	h := &K8sHelper{Log: logr.Discard()}

	fw := watch.NewFake()
	go func() {
		fw.Delete(readySandbox("sb"))
	}()

	var lastCond string
	_, done, err := h.drainSandboxWatch(context.Background(), fw, "test-claim", 5*time.Second, &lastCond)
	if done {
		t.Error("expected done=false for Deleted event")
	}
	if !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("expected ErrSandboxDeleted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle semaphore context-cancelled
// ---------------------------------------------------------------------------

func TestOpen_ContextCancelledAtSemaphore(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)

	c.lifecycleSem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.Open(ctx)
	if err == nil {
		t.Fatal("expected error when semaphore is held and context expires")
	}

	<-c.lifecycleSem
}

func TestClose_ContextCancelledAtSemaphore(t *testing.T) {
	opts := defaultTestOpts()
	opts.CleanupTimeout = 100 * time.Millisecond
	c, _, _ := newTestSandbox(opts)

	c.lifecycleSem <- struct{}{}

	err := c.Close(context.Background())
	if err == nil {
		t.Fatal("expected error when semaphore is held and cleanup times out")
	}

	<-c.lifecycleSem
}

func TestClose_CallerCtxCancelledWhileSemaphoreHeld(t *testing.T) {
	opts := defaultTestOpts()
	opts.CleanupTimeout = 5 * time.Second
	c, _, _ := newTestSandbox(opts)

	c.lifecycleSem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.Close(ctx)
	if err == nil {
		t.Fatal("expected error when caller's context expires while semaphore is held")
	}
	if !strings.Contains(err.Error(), "close cancelled") {
		t.Errorf("expected 'close cancelled' error, got: %v", err)
	}

	<-c.lifecycleSem
}

// ---------------------------------------------------------------------------
// Close error propagation
// ---------------------------------------------------------------------------

func TestClose_DeleteFailure_ReturnsError(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("server unavailable")
	})

	err := c.Close(context.Background())
	if err == nil {
		t.Fatal("expected error from Close when delete fails")
	}
}

func TestClose_CancelsBlockedOpen(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 30 * time.Second
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	claimCreated := make(chan struct{}, 1)
	extensionsCS.PrependReactor("create", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		select {
		case claimCreated <- struct{}{}:
		default:
		}
		return false, nil, nil
	})

	openDone := make(chan error, 1)
	go func() {
		openDone <- c.Open(context.Background())
	}()

	<-claimCreated

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	if err := <-openDone; err == nil {
		t.Fatal("expected Open() to return error after Close() cancelled it")
	}

	if c.ClaimName() != "" {
		t.Errorf("expected empty ClaimName after cleanup, got %q", c.ClaimName())
	}
}

// ---------------------------------------------------------------------------
// monitorPortForward tests (TunnelStrategy)
// ---------------------------------------------------------------------------

func TestMonitorPortForward_ClearsStateOnDeath(t *testing.T) {
	conn := newConnector(connectorConfig{
		Strategy:          &DirectStrategy{URL: "http://127.0.0.1:12345"},
		Namespace:         "default",
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Log:               logr.Discard(),
	})
	conn.mu.Lock()
	conn.baseURL = "http://127.0.0.1:12345"
	conn.mu.Unlock()

	stopChan := make(chan struct{})
	errChan := make(chan error, 1)
	stderrBuf := &syncBuffer{}

	ts := &tunnelStrategy{
		connector:           conn,
		log:                 logr.Discard(),
		portForwardStopChan: stopChan,
	}

	go ts.monitorPortForward(errChan, stderrBuf, "test-pod", stopChan)

	errChan <- fmt.Errorf("connection lost")

	deadline := time.After(2 * time.Second)
	for conn.BaseURL() != "" {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for monitorPortForward to clear baseURL")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !errors.Is(conn.lastError, ErrPortForwardDied) {
		t.Errorf("expected ErrPortForwardDied in lastError, got %v", conn.lastError)
	}
}

func TestMonitorPortForward_StaleMonitorIgnored(t *testing.T) {
	conn := newConnector(connectorConfig{
		Strategy:          &DirectStrategy{URL: "http://127.0.0.1:12345"},
		Namespace:         "default",
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Log:               logr.Discard(),
	})
	conn.mu.Lock()
	conn.baseURL = "http://127.0.0.1:12345"
	conn.mu.Unlock()

	oldStopChan := make(chan struct{})
	newStopChan := make(chan struct{})
	errChan := make(chan error, 1)
	stderrBuf := &syncBuffer{}

	ts := &tunnelStrategy{
		connector:           conn,
		log:                 logr.Discard(),
		portForwardStopChan: newStopChan,
	}

	done := make(chan struct{})
	go func() {
		ts.monitorPortForward(errChan, stderrBuf, "test-pod", oldStopChan)
		close(done)
	}()

	errChan <- fmt.Errorf("old connection lost")
	<-done

	if conn.BaseURL() != "http://127.0.0.1:12345" {
		t.Error("stale monitor should not have cleared baseURL")
	}
}

// ---------------------------------------------------------------------------
// Field selector validation
// ---------------------------------------------------------------------------

func TestOpen_WatchUsesCorrectFieldSelector(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, _ := newTestSandbox(opts)

	var capturedSelector string
	agentsCS.PrependWatchReactor("sandboxes", func(action ktesting.Action) (bool, watch.Interface, error) {
		wa := action.(ktesting.WatchAction)
		capturedSelector = wa.GetWatchRestrictions().Fields.String()
		// Extract claim name from field selector to satisfy the watch name check.
		name := strings.TrimPrefix(capturedSelector, "metadata.name=")
		fw := watch.NewFake()
		go fw.Add(readySandbox(name))
		return true, fw, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if capturedSelector == "" {
		t.Fatal("expected watch to have a field selector")
	}
	if !strings.Contains(capturedSelector, c.ClaimName()) {
		t.Errorf("expected field selector to contain claim name %q, got %q", c.ClaimName(), capturedSelector)
	}
}

func TestOpen_CreateClaimSendsCorrectTemplate(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	var capturedTemplate string
	fakeWatcher := watch.NewFake()

	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		ca := action.(ktesting.CreateAction)
		claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim)
		if ok {
			capturedTemplate = claim.Spec.TemplateRef.Name
			go fakeWatcher.Add(readySandbox(claim.Name))
		}
		return false, nil, nil
	})

	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if capturedTemplate != opts.TemplateName {
		t.Errorf("expected template name %q, got %q", opts.TemplateName, capturedTemplate)
	}
}

// ---------------------------------------------------------------------------
// Rollback failure path
// ---------------------------------------------------------------------------

func TestOpen_RollbackFailure_PreservesClaimName(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 200 * time.Millisecond
	c, agentsCS, extensionsCS := newTestSandbox(opts)

	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("api server unavailable")
	})

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected error from Open")
	}

	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout in error, got: %v", err)
	}

	if c.ClaimName() == "" {
		t.Error("expected ClaimName to be preserved after failed rollback")
	}
}

// ---------------------------------------------------------------------------
// Close retains claimName on delete failure
// ---------------------------------------------------------------------------

func TestClose_DeleteFailure_RetainsClaimName(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	claimName := c.ClaimName()

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("server unavailable")
	})

	err := c.Close(context.Background())
	if err == nil {
		t.Fatal("expected error from Close when delete fails")
	}

	if c.ClaimName() != claimName {
		t.Errorf("expected ClaimName %q to be retained after failed Close, got %q", claimName, c.ClaimName())
	}
	if c.SandboxName() != "" {
		t.Error("expected SandboxName to be cleared after failed Close")
	}
}

// ---------------------------------------------------------------------------
// Gateway watch drain error paths (GatewayStrategy)
// ---------------------------------------------------------------------------

func TestDrainGatewayWatch_ErrorTriggersRelist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status metav1.Status
	}{
		{"410Gone", metav1.Status{Code: 410, Reason: metav1.StatusReasonGone}},
		{"500Internal", metav1.Status{Code: 500, Message: "internal error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &gatewayStrategy{gatewayName: "test-gw", gatewayScheme: "http", log: logr.Discard()}
			fw := watch.NewFake()
			go func() { fw.Error(&tc.status) }()

			_, done, err := g.drainWatch(context.Background(), fw)
			if done {
				t.Error("expected done=false")
			}
			if err != nil {
				t.Errorf("expected nil error (should trigger re-list), got: %v", err)
			}
		})
	}
}

func TestDrainGatewayWatch_Deleted(t *testing.T) {
	g := &gatewayStrategy{gatewayName: "test-gw", gatewayScheme: "http", log: logr.Discard()}

	fw := watch.NewFake()
	go func() {
		fw.Delete(&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "gateway.networking.k8s.io/v1",
				"kind":       "Gateway",
				"metadata":   map[string]interface{}{"name": "test-gw"},
			},
		})
	}()

	_, done, err := g.drainWatch(context.Background(), fw)
	if done {
		t.Error("expected done=false for Deleted event")
	}
	if !errors.Is(err, ErrGatewayDeleted) {
		t.Errorf("expected ErrGatewayDeleted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Guard clause tests (strategy-level)
// ---------------------------------------------------------------------------

func TestWaitForGatewayIP_NilDynamicClient(t *testing.T) {
	g := &gatewayStrategy{
		gatewayName:      "test-gateway",
		gatewayNamespace: "default",
		gatewayScheme:    "http",
		timeout:          5 * time.Second,
		log:              logr.Discard(),
		tracer:           otel.GetTracerProvider().Tracer("test"),
	}
	_, err := g.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for nil dynamic client")
	}
	if !strings.Contains(err.Error(), "dynamic client required") {
		t.Errorf("expected dynamic client error, got: %v", err)
	}
}

func TestStartPortForward_NilCoreClient(t *testing.T) {
	ts := &tunnelStrategy{
		namespace: "default",
		log:       logr.Discard(),
		tracer:    otel.GetTracerProvider().Tracer("test"),
	}
	_, err := ts.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for nil core client")
	}
	if !strings.Contains(err.Error(), "core client and REST config required") {
		t.Errorf("expected core client error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Options validation
// ---------------------------------------------------------------------------

func TestValidation_NegativeTimeouts(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"negative SandboxReadyTimeout", Options{TemplateName: "t", SandboxReadyTimeout: -1}},
		{"negative GatewayReadyTimeout", Options{TemplateName: "t", GatewayReadyTimeout: -1}},
		{"negative PortForwardReadyTimeout", Options{TemplateName: "t", PortForwardReadyTimeout: -1}},
		{"negative CleanupTimeout", Options{TemplateName: "t", CleanupTimeout: -1}},
		{"negative RequestTimeout", Options{TemplateName: "t", RequestTimeout: -1}},
		{"negative PerAttemptTimeout", Options{TemplateName: "t", PerAttemptTimeout: -1}},
		{"negative MaxDownloadSize", Options{TemplateName: "t", MaxDownloadSize: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.setDefaults()
			if err := tc.opts.validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestValidation_InvalidNames(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"uppercase GatewayName", Options{TemplateName: "t", GatewayName: "MyGateway"}},
		{"uppercase Namespace", Options{TemplateName: "t", Namespace: "MyNS"}},
		{"uppercase TemplateName", Options{TemplateName: "MyTemplate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.setDefaults()
			if err := tc.opts.validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gateway timeout verifies claim cleanup
// ---------------------------------------------------------------------------

func TestGatewayDiscovery_Timeout_CleansUpClaim(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "missing-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 200 * time.Millisecond,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	gwWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("gateways", ktesting.DefaultWatchReactor(gwWatcher, nil))

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected timeout error for gateway discovery")
	}

	found := false
	for _, action := range extensionsCS.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "sandboxclaims" {
			found = true
		}
	}
	if !found {
		t.Error("expected a delete action for the SandboxClaim during gateway timeout rollback")
	}
}

// ---------------------------------------------------------------------------
// Reconnect after port-forward death
// ---------------------------------------------------------------------------

func TestOpen_ReconnectsAfterTransportDeath(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	sb := readySandbox("sb")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, sb, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	// Simulate port-forward death: clear baseURL but keep claim/sandbox set.
	c.connector.mu.Lock()
	c.connector.baseURL = ""
	c.connector.mu.Unlock()

	if c.IsReady() {
		t.Error("expected IsReady()=false after transport death")
	}

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() reconnect error: %v", err)
	}

	if !c.IsReady() {
		t.Error("expected IsReady()=true after reconnect")
	}
	if c.connector.BaseURL() != opts.APIURL {
		t.Errorf("expected baseURL=%s after reconnect, got %s", opts.APIURL, c.connector.BaseURL())
	}

	c.Close(context.Background())
}

// ---------------------------------------------------------------------------
// Annotations accessor
// ---------------------------------------------------------------------------

func TestAnnotations_ReturnsAnnotations(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	ann := c.Annotations()
	if ann == nil {
		t.Fatal("expected non-nil annotations")
	}
	expectedPod := c.ClaimName() + "-pod"
	if ann[PodNameAnnotation] != expectedPod {
		t.Errorf("expected pod annotation %s, got %s", expectedPod, ann[PodNameAnnotation])
	}
}

func TestAnnotations_ReturnsCopy(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	ann := c.Annotations()
	ann["mutated"] = "yes"

	ann2 := c.Annotations()
	if _, ok := ann2["mutated"]; ok {
		t.Error("Annotations() should return a copy, not a reference to internal state")
	}
}

func TestAnnotations_NilBeforeOpen(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)
	if ann := c.Annotations(); ann != nil {
		t.Errorf("expected nil annotations before Open, got %v", ann)
	}
}

// ---------------------------------------------------------------------------
// Name validation boundary tests
// ---------------------------------------------------------------------------

func TestIsValidDNSSubdomain(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"", false},
		{"a", true},
		{"abc", true},
		{"abc-def", true},
		{"-abc", false},
		{"abc-", false},
		{"ABC", false},
		{"abc.def", true},
		{"a.b.c", true},
		{".abc", false},
		{"abc.", false},
		{"abc_def", false},
		{"123", true},
		{"a2b", true},
		{"python-3.11", true},
		{strings.Repeat("a", 253), true},
		{strings.Repeat("a", 254), false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			if got := isValidDNSSubdomain(tc.name); got != tc.valid {
				t.Errorf("isValidDNSSubdomain(%q) = %v, want %v", tc.name, got, tc.valid)
			}
		})
	}
}

func TestIsValidDNSLabel(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"", false},
		{"a", true},
		{"abc", true},
		{"abc-def", true},
		{"-abc", false},
		{"abc-", false},
		{"ABC", false},
		{"abc.def", false},
		{"abc_def", false},
		{"123", true},
		{"a2b", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			if got := isValidDNSLabel(tc.name); got != tc.valid {
				t.Errorf("isValidDNSLabel(%q) = %v, want %v", tc.name, got, tc.valid)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Watch() call failure (non-context error)
// ---------------------------------------------------------------------------

func TestOpen_WatchError_NonContext(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 500 * time.Millisecond
	c, agentsCS, _ := newTestSandbox(opts)

	agentsCS.PrependWatchReactor("sandboxes", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, nil, fmt.Errorf("RBAC: forbidden")
	})

	err := c.Open(context.Background())
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected error from persistent Watch failure")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout after persistent watch failures, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Non-sandbox object in watch
// ---------------------------------------------------------------------------

func TestDrainSandboxWatch_NonSandboxObject(t *testing.T) {
	h := &K8sHelper{Log: logr.Discard()}

	fw := watch.NewFake()
	go func() {
		fw.Add(&metav1.Status{Message: "not a sandbox"})
		fw.Stop()
	}()

	var lastCond string
	_, done, err := h.drainSandboxWatch(context.Background(), fw, "test-claim", 5*time.Second, &lastCond)
	if done {
		t.Error("expected done=false for non-sandbox object")
	}
	if err != nil {
		t.Errorf("expected nil error when watch closes after non-sandbox object, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// syncBuffer truncation tests
// ---------------------------------------------------------------------------

func TestSyncBuffer_Truncation(t *testing.T) {
	b := &syncBuffer{}

	data := make([]byte, maxStderrSize)
	for i := range data {
		data[i] = 'A'
	}
	n, err := b.Write(data)
	if n != maxStderrSize || err != nil {
		t.Fatalf("Write(%d bytes) = (%d, %v), want (%d, nil)", maxStderrSize, n, err, maxStderrSize)
	}

	extra := []byte("overflow data")
	n, err = b.Write(extra)
	if n != len(extra) {
		t.Errorf("Write overflow returned n=%d, want %d", n, len(extra))
	}
	if err != nil {
		t.Errorf("Write overflow returned err=%v, want nil", err)
	}

	b.mu.Lock()
	bufLen := b.buf.Len()
	b.mu.Unlock()
	if bufLen != maxStderrSize {
		t.Errorf("buffer len = %d, want %d", bufLen, maxStderrSize)
	}

	s := b.String()
	if !strings.Contains(s, "[truncated at 64KB]") {
		t.Error("expected truncation marker in String() output")
	}
}

func TestSyncBuffer_PartialTruncation(t *testing.T) {
	b := &syncBuffer{}

	data := make([]byte, maxStderrSize-10)
	n, err := b.Write(data)
	if n != len(data) || err != nil {
		t.Fatalf("Write = (%d, %v)", n, err)
	}

	overflow := make([]byte, 20)
	n, err = b.Write(overflow)
	if n != 20 {
		t.Errorf("Write(overflow) returned n=%d, want 20", n)
	}
	if err != nil {
		t.Errorf("Write(overflow) returned err=%v, want nil", err)
	}

	b.mu.Lock()
	bufLen := b.buf.Len()
	b.mu.Unlock()
	if bufLen != maxStderrSize {
		t.Errorf("buffer len = %d, want %d", bufLen, maxStderrSize)
	}

	s := b.String()
	if !strings.Contains(s, "[truncated at 64KB]") {
		t.Error("expected truncation marker")
	}
}

// ---------------------------------------------------------------------------
// Gateway mode reconnect
// ---------------------------------------------------------------------------

func TestOpen_ReconnectsAfterTransportDeath_GatewayMode(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 5 * time.Second,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	var lastSb *sandboxv1alpha1.Sandbox
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				lastSb = readySandbox(claim.Name)
				go sandboxWatcher.Add(lastSb)
				return false, nil, nil
			}
		}
		return false, nil, nil
	})
	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if lastSb != nil {
			return true, lastSb, nil
		}
		return true, readySandbox("sb"), nil
	})

	var gwMu sync.Mutex
	gwCallCount := 0
	dynCS.PrependWatchReactor("gateways", func(_ ktesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFake()
		gwMu.Lock()
		gwCallCount++
		ip := fmt.Sprintf("203.0.113.%d", gwCallCount)
		gwMu.Unlock()
		go fw.Add(&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "gateway.networking.k8s.io/v1",
				"kind":       "Gateway",
				"metadata":   map[string]interface{}{"name": "test-gateway", "namespace": "default"},
				"status":     map[string]interface{}{"addresses": []interface{}{map[string]interface{}{"value": ip}}},
			},
		})
		return true, fw, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if c.connector.BaseURL() != "http://203.0.113.1" {
		t.Errorf("expected first gateway IP, got %s", c.connector.BaseURL())
	}

	// Simulate transport death.
	c.connector.mu.Lock()
	c.connector.baseURL = ""
	c.connector.mu.Unlock()

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() reconnect error: %v", err)
	}
	if c.connector.BaseURL() != "http://203.0.113.2" {
		t.Errorf("expected second gateway IP after reconnect, got %s", c.connector.BaseURL())
	}
	c.Close(context.Background())
}

// ---------------------------------------------------------------------------
// Gateway list-path test (IP already available before watch)
// ---------------------------------------------------------------------------

func TestGatewayDiscovery_ListPath_AlreadyReady(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		ServerPort:          8888,
		SandboxReadyTimeout: 5 * time.Second,
		GatewayReadyTimeout: 5 * time.Second,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	sandboxWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(sandboxWatcher, nil))
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				go sandboxWatcher.Add(readySandbox(claim.Name))
				return false, nil, nil
			}
		}
		return false, nil, nil
	})

	dynCS.PrependReactor("list", "gateways", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{
			Object: map[string]interface{}{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayList"},
			Items: []unstructured.Unstructured{{
				Object: map[string]interface{}{
					"apiVersion": "gateway.networking.k8s.io/v1",
					"kind":       "Gateway",
					"metadata":   map[string]interface{}{"name": "test-gateway", "namespace": "default"},
					"status":     map[string]interface{}{"addresses": []interface{}{map[string]interface{}{"value": "198.51.100.1"}}},
				},
			}},
		}, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() with gateway list-path error: %v", err)
	}
	defer c.Close(context.Background())

	if c.connector.BaseURL() != "http://198.51.100.1" {
		t.Errorf("expected gateway IP from list path, got %s", c.connector.BaseURL())
	}
}

// ---------------------------------------------------------------------------
// HTTPError structured error
// ---------------------------------------------------------------------------

func TestHTTPError_CanBeExtracted(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("file not found"))
	}))
	defer server.Close()

	c.connector.mu.Lock()
	c.connector.baseURL = server.URL
	c.connector.mu.Unlock()

	_, err := c.Read(context.Background(), "missing.txt")
	if err == nil {
		t.Fatal("expected error for 404")
	}

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected error to contain *HTTPError, got: %v", err)
	}
	if httpErr.StatusCode != 404 {
		t.Errorf("expected StatusCode=404, got %d", httpErr.StatusCode)
	}
	if httpErr.Operation != "read" {
		t.Errorf("expected Operation=read, got %s", httpErr.Operation)
	}
}

// ---------------------------------------------------------------------------
// ErrOrphanedClaim test
// ---------------------------------------------------------------------------

func TestOpen_OrphanedClaimReturnsError(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)

	c.mu.Lock()
	c.claimName = "orphaned-claim"
	c.mu.Unlock()

	err := c.Open(context.Background())
	if !errors.Is(err, ErrOrphanedClaim) {
		t.Fatalf("expected ErrOrphanedClaim, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// verifySandboxAlive failure during reconnect
// ---------------------------------------------------------------------------

func TestOpen_ReconnectFailsWhenSandboxGone(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	sb := readySandbox("sb")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewNotFound(
			schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"}, "sb")
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	claimName := c.ClaimName()

	c.connector.mu.Lock()
	c.connector.baseURL = ""
	c.connector.mu.Unlock()

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error from reconnect when sandbox is gone")
	}
	if !errors.Is(err, ErrOrphanedClaim) {
		t.Errorf("expected ErrOrphanedClaim, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no longer available") {
		t.Errorf("expected 'no longer available' error, got: %v", err)
	}
	if c.ClaimName() != claimName {
		t.Errorf("expected claimName %q preserved, got %q", claimName, c.ClaimName())
	}
	if c.SandboxName() != "" {
		t.Errorf("expected sandboxName cleared, got %q", c.SandboxName())
	}

	c.Close(context.Background())
}

// ---------------------------------------------------------------------------
// Reconnect: sandbox not-ready
// ---------------------------------------------------------------------------

func TestOpen_ReconnectFailsWhenSandboxNotReady(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	sb := readySandbox("sb")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	claimName := c.ClaimName()

	c.connector.mu.Lock()
	c.connector.baseURL = ""
	c.connector.mu.Unlock()

	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &sandboxv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
			Status: sandboxv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{Type: string(sandboxv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse},
				},
			},
		}, nil
	})

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error for not-ready sandbox during reconnect")
	}
	if !errors.Is(err, ErrOrphanedClaim) {
		t.Errorf("expected ErrOrphanedClaim, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no longer ready") {
		t.Errorf("expected 'no longer ready' error, got: %v", err)
	}
	if c.ClaimName() != claimName {
		t.Errorf("expected claimName %q preserved, got %q", claimName, c.ClaimName())
	}
	if c.SandboxName() == "" {
		t.Error("expected sandboxName preserved for non-NotFound error")
	}
	c.Close(context.Background())
}

// ---------------------------------------------------------------------------
// Reconnect: claim gone
// ---------------------------------------------------------------------------

func TestOpen_ReconnectFailsWhenClaimGone(t *testing.T) {
	opts := defaultTestOpts()
	c, _, extensionsCS := newTestSandbox(opts)

	c.mu.Lock()
	c.claimName = "test-claim-abc"
	c.sandboxName = "test-sandbox"
	c.mu.Unlock()

	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewNotFound(
			schema.GroupResource{Group: "extensions.agents.x-k8s.io", Resource: "sandboxclaims"},
			"test-claim-abc",
		)
	})

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error when claim is gone during reconnect")
	}
	if errors.Is(err, ErrOrphanedClaim) {
		t.Errorf("expected non-orphan error for 404 claim, got: %v", err)
	}
	if !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("expected ErrSandboxDeleted, got: %v", err)
	}
	if c.ClaimName() != "" {
		t.Errorf("expected claimName cleared for 404, got %q", c.ClaimName())
	}
	if c.SandboxName() != "" {
		t.Errorf("expected sandboxName cleared, got %q", c.SandboxName())
	}
}

// ---------------------------------------------------------------------------
// Close with cancelled context uses detached cleanup
// ---------------------------------------------------------------------------

func TestClose_CancelledContextUsesDetachedCleanup(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	claimName := c.ClaimName()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close() with cancelled context should still delete claim, got: %v", err)
	}

	found := false
	for _, action := range extensionsCS.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "sandboxclaims" {
			da, ok := action.(ktesting.DeleteAction)
			if ok && da.GetName() == claimName {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected claim to be deleted even with cancelled context")
	}
}

// ---------------------------------------------------------------------------
// APIURL validation
// ---------------------------------------------------------------------------

func TestValidation_InvalidAPIURL(t *testing.T) {
	cases := []struct {
		name   string
		apiURL string
	}{
		{"no scheme", "example.com:8080"},
		{"ftp scheme", "ftp://example.com"},
		{"no host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{TemplateName: "t", APIURL: tc.apiURL}
			opts.setDefaults()
			if err := opts.validate(); err == nil {
				t.Errorf("expected validation error for APIURL %q", tc.apiURL)
			}
		})
	}
}

func TestValidation_ValidAPIURL(t *testing.T) {
	cases := []string{
		"http://localhost:8080",
		"https://sandbox.example.com",
		"http://sandbox-router-svc.default.svc.cluster.local:8080",
	}
	for _, apiURL := range cases {
		t.Run(apiURL, func(t *testing.T) {
			opts := Options{TemplateName: "t", APIURL: apiURL}
			opts.setDefaults()
			if err := opts.validate(); err != nil {
				t.Errorf("unexpected validation error for APIURL %q: %v", apiURL, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// formatConditions tests
// ---------------------------------------------------------------------------

func TestFormatConditions(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{"empty", nil, "none observed"},
		{"single", []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}, "Ready=True"},
		{"multiple", []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue},
			{Type: "Scheduled", Status: metav1.ConditionFalse},
		}, "Ready=True, Scheduled=False"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatConditions(tc.conditions)
			if got != tc.expected {
				t.Errorf("formatConditions() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// setState clears stale annotations
// ---------------------------------------------------------------------------

func TestSetState_ClearsStaleAnnotations(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)

	c.setState(&sandboxState{
		SandboxName: "sb1",
		PodName:     "pod1",
		Annotations: map[string]string{
			PodNameAnnotation: "pod1",
			"custom":          "value",
		},
	})
	if c.Annotations() == nil {
		t.Fatal("expected non-nil annotations after first setState")
	}
	if c.Annotations()["custom"] != "value" {
		t.Errorf("expected custom annotation, got %v", c.Annotations())
	}

	c.setState(&sandboxState{
		SandboxName: "sb2",
		PodName:     "sb2",
	})
	if c.Annotations() != nil {
		t.Errorf("expected nil annotations after setState with nil, got %v", c.Annotations())
	}
	if c.PodName() != "sb2" {
		t.Errorf("expected PodName fallback to sb2, got %s", c.PodName())
	}
}

// ---------------------------------------------------------------------------
// Watch re-list loop after watch close
// ---------------------------------------------------------------------------

func TestOpen_WatchClosedReListFinds(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 5 * time.Second
	c, agentsCS, _ := newTestSandbox(opts)

	// Track the claim name once it's created so the watch/list reactors
	// can return a sandbox with a matching name.
	var claimMu sync.Mutex
	var claimName string

	var watchCount sync.Mutex
	watchN := 0
	agentsCS.PrependWatchReactor("sandboxes", func(action ktesting.Action) (bool, watch.Interface, error) {
		wa := action.(ktesting.WatchAction)
		name := strings.TrimPrefix(wa.GetWatchRestrictions().Fields.String(), "metadata.name=")
		claimMu.Lock()
		claimName = name
		claimMu.Unlock()
		fw := watch.NewFake()
		watchCount.Lock()
		watchN++
		n := watchN
		watchCount.Unlock()
		if n == 1 {
			go fw.Stop()
		} else {
			go fw.Add(readySandbox(name))
		}
		return true, fw, nil
	})

	var listMu sync.Mutex
	listN := 0
	agentsCS.PrependReactor("list", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		listMu.Lock()
		listN++
		n := listN
		listMu.Unlock()
		claimMu.Lock()
		name := claimName
		claimMu.Unlock()
		if n == 1 {
			return true, &sandboxv1alpha1.SandboxList{}, nil
		}
		sb := readySandbox(name)
		return true, &sandboxv1alpha1.SandboxList{Items: []sandboxv1alpha1.Sandbox{*sb}}, nil
	})

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer c.Close(context.Background())

	if c.SandboxName() != c.ClaimName() {
		t.Errorf("expected SandboxName=%s to match ClaimName=%s", c.SandboxName(), c.ClaimName())
	}
}

// ---------------------------------------------------------------------------
// monitorPortForward timeout test
// ---------------------------------------------------------------------------

func TestMonitorPortForward_Timeout(t *testing.T) {
	conn := newConnector(connectorConfig{
		Strategy:          &DirectStrategy{URL: "http://127.0.0.1:12345"},
		Namespace:         "default",
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Log:               logr.Discard(),
	})
	conn.mu.Lock()
	conn.baseURL = "http://127.0.0.1:12345"
	conn.mu.Unlock()

	stopChan := make(chan struct{})
	errChan := make(chan error, 1)
	stderrBuf := &syncBuffer{}

	ts := &tunnelStrategy{
		connector:           conn,
		log:                 logr.Discard(),
		portForwardStopChan: stopChan,
	}

	done := make(chan struct{})
	go func() {
		ts.monitorPortForward(errChan, stderrBuf, "test-pod", stopChan)
		close(done)
	}()

	close(stopChan)

	select {
	case <-done:
	case <-time.After(monitorExitTimeout + 5*time.Second):
		t.Fatal("monitorPortForward did not exit after stop signal timeout")
	}

	if conn.BaseURL() != "" {
		t.Errorf("expected baseURL cleared after monitor timeout, got %q", conn.BaseURL())
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.lastError == nil {
		t.Error("expected lastError to be set after monitor timeout")
	}
}

// ---------------------------------------------------------------------------
// drainGatewayWatch non-Unstructured object
// ---------------------------------------------------------------------------

func TestDrainGatewayWatch_NonUnstructuredObject(t *testing.T) {
	g := &gatewayStrategy{gatewayName: "test-gw", gatewayScheme: "http", log: logr.Discard()}

	fw := watch.NewFake()
	go func() {
		fw.Add(&metav1.Status{Message: "not a gateway"})
		fw.Stop()
	}()

	_, done, err := g.drainWatch(context.Background(), fw)
	if done {
		t.Error("expected done=false for non-Unstructured object")
	}
	if err != nil {
		t.Errorf("expected nil error when watch closes after non-Unstructured object, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// monitorPortForward timeout sets lastError
// ---------------------------------------------------------------------------

func TestMonitorPortForward_Timeout_SetsLastError(t *testing.T) {
	conn := newConnector(connectorConfig{
		Strategy:          &DirectStrategy{URL: "http://127.0.0.1:12345"},
		Namespace:         "default",
		ServerPort:        8888,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Log:               logr.Discard(),
	})
	conn.mu.Lock()
	conn.baseURL = "http://127.0.0.1:12345"
	conn.mu.Unlock()

	stopChan := make(chan struct{})
	errChan := make(chan error, 1)
	stderrBuf := &syncBuffer{}

	ts := &tunnelStrategy{
		connector:           conn,
		log:                 logr.Discard(),
		portForwardStopChan: stopChan,
	}

	done := make(chan struct{})
	go func() {
		ts.monitorPortForward(errChan, stderrBuf, "test-pod", stopChan)
		close(done)
	}()

	close(stopChan)

	select {
	case <-done:
	case <-time.After(monitorExitTimeout + 5*time.Second):
		t.Fatal("monitorPortForward did not exit after stop signal timeout")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.lastError == nil {
		t.Error("expected lastError to be set after monitor timeout")
	}
	if conn.lastError != nil && !errors.Is(conn.lastError, ErrPortForwardDied) {
		t.Errorf("expected ErrPortForwardDied in lastError, got: %v", conn.lastError)
	}
}

// ---------------------------------------------------------------------------
// Close delete failure preserves reconnect state
// ---------------------------------------------------------------------------

func TestClose_DeleteFailure_ClearsIdentityButKeepsClaim(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	if err := c.Open(context.Background()); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	claimName := c.ClaimName()

	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("server unavailable")
	})

	err := c.Close(context.Background())
	if err == nil {
		t.Fatal("expected error from Close when delete fails")
	}

	if c.ClaimName() != claimName {
		t.Errorf("expected ClaimName %q to be preserved, got %q", claimName, c.ClaimName())
	}
	if c.SandboxName() != "" {
		t.Error("expected SandboxName to be cleared after failed Close")
	}
	if c.PodName() != "" {
		t.Error("expected PodName to be cleared after failed Close")
	}
	if err := c.Open(context.Background()); !errors.Is(err, ErrOrphanedClaim) {
		t.Fatalf("expected ErrOrphanedClaim after failed Close, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GatewayScheme validation
// ---------------------------------------------------------------------------

func TestValidation_InvalidGatewayScheme(t *testing.T) {
	opts := Options{TemplateName: "t", GatewayScheme: "ftp"}
	opts.setDefaults()
	if err := opts.validate(); err == nil {
		t.Fatal("expected error for invalid GatewayScheme")
	}
}

// ---------------------------------------------------------------------------
// In-flight drain mechanism
// ---------------------------------------------------------------------------

func TestClose_DrainsInflightBeforeDelete(t *testing.T) {
	requestReceived := make(chan struct{})
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestReceived)
		<-unblock
		_, _ = w.Write([]byte(`{"exists":true}`))
	}))
	defer server.Close()

	opts := defaultTestOpts()
	opts.CleanupTimeout = 5 * time.Second
	c, _, extensionsCS := newTestSandbox(opts)

	c.connector.mu.Lock()
	c.connector.baseURL = server.URL
	c.connector.claimName = "drain-test-claim"
	c.connector.mu.Unlock()
	c.mu.Lock()
	c.claimName = "drain-test-claim"
	c.mu.Unlock()

	opDone := make(chan struct{})
	extensionsCS.PrependReactor("delete", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		select {
		case <-opDone:
		default:
			t.Error("claim deleted while in-flight operation still running")
		}
		return false, nil, nil
	})

	go func() {
		_, _ = c.Exists(context.Background(), "test.txt")
		close(opDone)
	}()

	<-requestReceived
	time.AfterFunc(100*time.Millisecond, func() { close(unblock) })

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	select {
	case <-opDone:
	default:
		t.Error("operation did not complete")
	}
}

// ---------------------------------------------------------------------------
// Reconnect transport failure
// ---------------------------------------------------------------------------

func TestOpen_ReconnectTransportFailure(t *testing.T) {
	opts := Options{
		TemplateName:        "test-template",
		Namespace:           "default",
		GatewayName:         "test-gateway",
		GatewayNamespace:    "default",
		SandboxReadyTimeout: 2 * time.Second,
		GatewayReadyTimeout: 200 * time.Millisecond,
		Quiet:               true,
	}
	opts.setDefaults()

	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList",
	})
	c, agentsCS, extensionsCS := newTestSandboxWithDynamic(opts, dynCS)

	c.mu.Lock()
	c.claimName = "existing-claim"
	c.sandboxName = "existing-sandbox"
	c.mu.Unlock()

	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &extv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "existing-claim", Namespace: "default"},
		}, nil
	})
	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, readySandbox("existing-sandbox"), nil
	})
	dynCS.PrependReactor("list", "gateways", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{
			Object: map[string]interface{}{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayList"},
		}, nil
	})
	dynCS.PrependWatchReactor("gateways", ktesting.DefaultWatchReactor(watch.NewFake(), nil))

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error from reconnect transport failure")
	}
	if !strings.Contains(err.Error(), "reconnect transport failed") {
		t.Errorf("expected 'reconnect transport failed', got: %v", err)
	}
	if c.SandboxName() == "" {
		t.Error("expected sandboxName preserved for retry")
	}
	if c.ClaimName() == "" {
		t.Error("expected claimName preserved for retry")
	}
}

// ---------------------------------------------------------------------------
// Reconnect transient claim error
// ---------------------------------------------------------------------------

func TestOpen_ReconnectTransientClaimError(t *testing.T) {
	opts := defaultTestOpts()
	c, _, extensionsCS := newTestSandbox(opts)

	c.mu.Lock()
	c.claimName = "existing-claim"
	c.sandboxName = "existing-sandbox"
	c.mu.Unlock()

	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	err := c.Open(context.Background())
	if err == nil {
		t.Fatal("expected error from transient claim verification")
	}
	if !errors.Is(err, ErrOrphanedClaim) {
		t.Errorf("expected ErrOrphanedClaim, got: %v", err)
	}
	if c.ClaimName() == "" {
		t.Error("expected claimName preserved for Close retry")
	}
	if c.SandboxName() != "" {
		t.Error("expected sandboxName cleared on transient error")
	}
}

// ---------------------------------------------------------------------------
// Disconnect tests
// ---------------------------------------------------------------------------

func TestDisconnect_AlreadyDisconnected(t *testing.T) {
	opts := defaultTestOpts()
	c, _, _ := newTestSandbox(opts)

	// Disconnect on an unopened sandbox should be a safe no-op.
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect on unopened sandbox should succeed, got: %v", err)
	}
	if c.IsReady() {
		t.Error("expected IsReady()=false")
	}
}

func TestDisconnect_ThenReconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	opts := defaultTestOpts()
	opts.APIURL = srv.URL
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	claimName := c.ClaimName()
	sandboxName := c.SandboxName()

	// Disconnect: transport closes, but claim/sandbox identity preserved.
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect() error: %v", err)
	}
	if c.IsReady() {
		t.Error("expected IsReady()=false after Disconnect")
	}
	if c.ClaimName() != claimName {
		t.Errorf("expected claimName preserved after Disconnect, got %q", c.ClaimName())
	}
	if c.SandboxName() != sandboxName {
		t.Errorf("expected sandboxName preserved after Disconnect, got %q", c.SandboxName())
	}

	// Wire reactors for reconnect: get claim, get sandbox, watch.
	extensionsCS.PrependReactor("get", "sandboxclaims", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &extv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default"},
		}, nil
	})
	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, readySandbox(sandboxName), nil
	})

	// Reconnect via Open().
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() after Disconnect error: %v", err)
	}
	defer c.Close(context.Background())

	if !c.IsReady() {
		t.Error("expected IsReady()=true after reconnect")
	}
	if c.ClaimName() != claimName {
		t.Error("expected same claimName after reconnect")
	}
}

func TestDisconnect_CancelsBlockedOpen(t *testing.T) {
	opts := defaultTestOpts()
	opts.SandboxReadyTimeout = 2 * time.Second
	c, agentsCS, _ := newTestSandbox(opts)

	// Watcher that never sends a ready sandbox — Open blocks.
	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	openErr := make(chan error, 1)
	go func() {
		openErr <- c.Open(context.Background())
	}()

	// Give Open time to start blocking on watch.
	time.Sleep(100 * time.Millisecond)

	// Disconnect cancels the in-progress Open.
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect() error: %v", err)
	}

	err := <-openErr
	if err == nil {
		c.Close(context.Background())
		t.Fatal("expected Open to fail after Disconnect cancelled it")
	}
}

func TestDisconnect_Timeout(t *testing.T) {
	opts := defaultTestOpts()
	opts.CleanupTimeout = 100 * time.Millisecond
	c, _, _ := newTestSandbox(opts)

	// Hold the lifecycle semaphore so Disconnect times out waiting.
	c.lifecycleSem <- struct{}{}

	err := c.Disconnect(context.Background())
	if err == nil {
		t.Fatal("expected timeout error from Disconnect")
	}
	if !strings.Contains(err.Error(), "disconnect timed out") {
		t.Errorf("expected disconnect timeout error, got: %v", err)
	}

	// Release the sem for cleanup.
	<-c.lifecycleSem
}

func TestDisconnect_DoubleDisconnect(t *testing.T) {
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("first Disconnect() error: %v", err)
	}
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("second Disconnect() should be safe no-op, got: %v", err)
	}

	// Claim should still be preserved for reconnect or Close.
	if c.ClaimName() == "" {
		t.Error("expected claimName preserved after double Disconnect")
	}

	// Clean up.
	c.Close(context.Background())
}
