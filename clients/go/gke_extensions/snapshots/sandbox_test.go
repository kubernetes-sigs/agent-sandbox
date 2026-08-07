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

package snapshots

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandbox "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
)

// ---------------------------------------------------------------------------
// Minimal sandbox.Handle + sandbox.Info stub for testing
// ---------------------------------------------------------------------------

type stubHandle struct {
	closeErr error
	closed   bool
}

func (s *stubHandle) Open(_ context.Context) error       { return nil }
func (s *stubHandle) Close(_ context.Context) error      { s.closed = true; return s.closeErr }
func (s *stubHandle) Disconnect(_ context.Context) error { return nil }
func (s *stubHandle) IsReady() bool                      { return !s.closed }
func (s *stubHandle) Run(_ context.Context, _ string, _ ...sandbox.CallOption) (*sandbox.ExecutionResult, error) {
	return nil, nil
}
func (s *stubHandle) Write(_ context.Context, _ string, _ []byte, _ ...sandbox.CallOption) error {
	return nil
}
func (s *stubHandle) Read(_ context.Context, _ string, _ ...sandbox.CallOption) ([]byte, error) {
	return nil, nil
}
func (s *stubHandle) List(_ context.Context, _ string, _ ...sandbox.CallOption) ([]sandbox.FileEntry, error) {
	return nil, nil
}
func (s *stubHandle) Exists(_ context.Context, _ string, _ ...sandbox.CallOption) (bool, error) {
	return false, nil
}

type stubInfo struct {
	claimName   string
	sandboxName string
	podName     string
	annotations map[string]string
}

func (s *stubInfo) ClaimName() string              { return s.claimName }
func (s *stubInfo) SandboxName() string            { return s.sandboxName }
func (s *stubInfo) PodName() string                { return s.podName }
func (s *stubInfo) Annotations() map[string]string { return s.annotations }

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestK8sHelper(agentsCS *fakeagents.Clientset, dynCS *fakedynamic.FakeDynamicClient, kubeCS *fakekube.Clientset) *sandbox.K8sHelper {
	return &sandbox.K8sHelper{
		AgentsClient:  agentsCS.AgentsV1beta1(),
		DynamicClient: dynCS,
		CoreClient:    kubeCS.CoreV1(),
		Log:           logr.Discard(),
	}
}

func newTestSandboxWrapper(
	agentsCS *fakeagents.Clientset,
	dynCS *fakedynamic.FakeDynamicClient,
	kubeCS *fakekube.Clientset,
) *SandboxWithSnapshotSupport {
	handle := &stubHandle{}
	info := &stubInfo{
		claimName:   "my-claim",
		sandboxName: "my-sandbox",
		podName:     "my-pod",
	}
	k8s := newTestK8sHelper(agentsCS, dynCS, kubeCS)
	return NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())
}

func makeAgentsClientset(sb *sandboxv1beta1.Sandbox) *fakeagents.Clientset { //nolint:staticcheck
	cs := fakeagents.NewSimpleClientset() //nolint:staticcheck
	if sb != nil {
		_, err := cs.AgentsV1beta1().Sandboxes(sb.Namespace).Create(
			context.Background(), sb, metav1.CreateOptions{})
		if err != nil {
			panic("test setup: failed to create sandbox in fake clientset: " + err.Error())
		}
	}
	return cs
}

func makeSandbox(replicas int32, selector string) *sandboxv1beta1.Sandbox {
	r := replicas
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-sandbox",
			Namespace: "default",
		},
		Spec: sandboxv1beta1.SandboxSpec{
			Replicas: &r,
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{},
			},
		},
		Status: sandboxv1beta1.SandboxStatus{
			LabelSelector: selector,
		},
	}
}

func newTestDynClient() *fakedynamic.FakeDynamicClient {
	return fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			triggerGVR:  "PodSnapshotManualTriggerList",
			snapshotGVR: "PodSnapshotList",
		},
	)
}

// ---------------------------------------------------------------------------
// IsSuspended
// ---------------------------------------------------------------------------

func TestIsSuspended_True(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=abc123")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	suspended, err := wrapper.IsSuspended(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !suspended {
		t.Error("expected sandbox to be suspended (replicas=0)")
	}
}

func TestIsSuspended_False(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=abc123")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	suspended, err := wrapper.IsSuspended(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended {
		t.Error("expected sandbox to be running (replicas=1)")
	}
}

func TestIsSuspended_NilReplicas(t *testing.T) {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sandbox", Namespace: "default"},
		Spec: sandboxv1beta1.SandboxSpec{
			Replicas:    nil, // unset → default 1
			PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{}},
		},
	}
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	suspended, err := wrapper.IsSuspended(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended {
		t.Error("expected not suspended when replicas is nil")
	}
}

// ---------------------------------------------------------------------------
// resolveSandboxNameHash
// ---------------------------------------------------------------------------

func TestResolveSandboxNameHash_Success(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=hash42")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	hash, err := wrapper.resolveSandboxNameHash(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != "hash42" {
		t.Errorf("expected hash42, got %q", hash)
	}
}

func TestResolveSandboxNameHash_Caches(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=cached")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	h1, _ := wrapper.resolveSandboxNameHash(context.Background())
	// Delete from k8s so a second call that hits the API would fail.
	agentsCS.AgentsV1beta1().Sandboxes("default").Delete(context.Background(), "my-sandbox", metav1.DeleteOptions{}) //nolint:errcheck
	h2, err := wrapper.resolveSandboxNameHash(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if h1 != h2 {
		t.Errorf("expected cached value %q, got %q", h1, h2)
	}
}

func TestResolveSandboxNameHash_EmptySelector(t *testing.T) {
	sb := makeSandbox(1, "")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	_, err := wrapper.resolveSandboxNameHash(context.Background())
	if err == nil {
		t.Error("expected error when status.selector is empty")
	}
}

func TestResolveSandboxNameHash_ErrorOnMalformedSelector(t *testing.T) {
	// Selector contains the wrong label key; expected label is absent.
	sb := makeSandbox(1, "some-other-label=abc123")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	_, err := wrapper.resolveSandboxNameHash(context.Background())
	if err == nil {
		t.Error("expected error when selector does not contain the expected label")
	}
}

// ---------------------------------------------------------------------------
// IsRestoredFromSnapshot
// ---------------------------------------------------------------------------

func TestIsRestoredFromSnapshot_EmptyUID(t *testing.T) {
	wrapper := newTestSandboxWrapper(
		makeAgentsClientset(nil), newTestDynClient(), fakekube.NewSimpleClientset())

	result := wrapper.IsRestoredFromSnapshot(context.Background(), "")
	if result.Success {
		t.Error("expected failure for empty snapshotUID")
	}
}

func TestIsRestoredFromSnapshot_NoPod(t *testing.T) {
	info := &stubInfo{sandboxName: "my-sandbox", podName: ""}
	handle := &stubHandle{}
	k8s := newTestK8sHelper(makeAgentsClientset(nil), newTestDynClient(), fakekube.NewSimpleClientset())
	wrapper := NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())

	result := wrapper.IsRestoredFromSnapshot(context.Background(), "snap-123")
	if result.Success {
		t.Error("expected failure when pod name is empty")
	}
}

// ---------------------------------------------------------------------------
// Close delegates cleanup to engine and underlying handle
// ---------------------------------------------------------------------------

func TestClose_DelegatesAndCleansUp(t *testing.T) {
	handle := &stubHandle{}
	info := &stubInfo{sandboxName: "my-sandbox", podName: "my-pod"}
	k8s := newTestK8sHelper(makeAgentsClientset(nil), newTestDynClient(), fakekube.NewSimpleClientset())
	wrapper := NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())

	// Initialise the engine so it's non-nil on Close.
	_ = wrapper.Snapshots()

	if err := wrapper.Close(context.Background()); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !handle.closed {
		t.Error("expected underlying handle.Close() to be called")
	}
}

func TestClose_PropagatesHandleError(t *testing.T) {
	wantErr := errors.New("close failed")
	handle := &stubHandle{closeErr: wantErr}
	info := &stubInfo{sandboxName: "my-sandbox", podName: "my-pod"}
	k8s := newTestK8sHelper(makeAgentsClientset(nil), newTestDynClient(), fakekube.NewSimpleClientset())
	wrapper := NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())

	if err := wrapper.Close(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("expected close error %v, got %v", wantErr, err)
	}
}

// ---------------------------------------------------------------------------
// Suspend / Resume (basic state check)
// ---------------------------------------------------------------------------

func TestSuspend_AlreadySuspended(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if !resp.Success {
		t.Errorf("expected success when already suspended, got: %s", resp.ErrorReason)
	}
}

func TestResume_AlreadyRunning(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if !resp.Success {
		t.Errorf("expected success when already running, got: %s", resp.ErrorReason)
	}
}

func TestSuspend_ScalesDownAndWaitsForTermination(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)
	// Pod doesn't exist → termination is immediate.
	kubeCS := fakekube.NewSimpleClientset()
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), kubeCS)

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success, got: %s", resp.ErrorReason)
	}

	// Verify sandbox was patched to replicas=0.
	updated, err := agentsCS.AgentsV1beta1().Sandboxes("default").Get(context.Background(), "my-sandbox", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting sandbox after suspend: %v", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 0 {
		t.Errorf("expected replicas=0 after suspend, got %v", updated.Spec.Replicas)
	}
}

func TestResume_ScalesUpAndWaitsForReady(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)

	// Pod exists and is Ready.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), kubeCS)

	// No snapshots exist → skip restore verification.
	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success, got: %s", resp.ErrorReason)
	}
	if resp.RestoredFromSnapshot {
		t.Error("expected RestoredFromSnapshot=false when no snapshots exist")
	}

	// Verify sandbox was patched to replicas=1.
	updated, err := agentsCS.AgentsV1beta1().Sandboxes("default").Get(context.Background(), "my-sandbox", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting sandbox after resume: %v", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 1 {
		t.Errorf("expected replicas=1 after resume, got %v", updated.Spec.Replicas)
	}
}

func TestSuspend_FailsWhenHashNotResolvable(t *testing.T) {
	// Sandbox CR has empty selector → resolveSandboxNameHash returns an error.
	sb := makeSandbox(1, "")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if resp.Success {
		t.Error("expected failure when sandbox-name-hash cannot be resolved")
	}
	if resp.ErrorCode != ErrorCode {
		t.Errorf("expected ErrorCode, got %d", resp.ErrorCode)
	}
}

func TestResume_Timeout(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)

	// Pod is not ready.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), kubeCS)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp := wrapper.Resume(ctx, 5*time.Second)
	if resp.Success {
		t.Error("expected failure on pod ready timeout")
	}
}

// ---------------------------------------------------------------------------
// Suspend — error paths
// ---------------------------------------------------------------------------

func TestSuspend_IsSuspendedError(t *testing.T) {
	// No sandbox CR → IsSuspended returns not-found error.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if resp.Success {
		t.Error("expected failure when sandbox CR does not exist")
	}
}

func TestSuspend_SetReplicasError(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)
	// Make patch fail.
	agentsCS.PrependReactor("patch", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("patch forbidden")
	})
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if resp.Success {
		t.Error("expected failure when setReplicas returns error")
	}
}

func TestSuspend_WithSnapshotSuccess(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)
	dynCS := newTestDynClient()

	// Snapshot trigger watch fires with completion.
	trigWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshotmanualtriggers", func(_ ktesting.Action) (bool, watch.Interface, error) {
		go func() {
			trigWatcher.Modify(&unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
					"kind":       PodSnapshotTriggerKind,
					"metadata":   map[string]any{"name": "t", "namespace": "default"},
					"status": map[string]any{
						"snapshotCreated": map[string]any{"name": "snap-uid"},
						"conditions": []any{
							map[string]any{
								"type":               "Triggered",
								"status":             "True",
								"reason":             "Complete",
								"lastTransitionTime": "2026-01-01T00:00:00Z",
							},
						},
					},
				},
			})
		}()
		return true, trigWatcher, nil
	})

	// Pod doesn't exist → termination is immediate.
	wrapper := newTestSandboxWrapper(agentsCS, dynCS, fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), true, 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success with snapshot, got: %s", resp.ErrorReason)
	}
	if resp.SnapshotResponse == nil {
		t.Error("expected SnapshotResponse to be set")
	}
}

func TestSuspend_WithSnapshotFail(t *testing.T) {
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)
	dynCS := newTestDynClient()

	// Snapshot trigger watch fires with failure.
	trigWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshotmanualtriggers", func(_ ktesting.Action) (bool, watch.Interface, error) {
		go func() {
			trigWatcher.Modify(&unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{"name": "t", "namespace": "default"},
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":    "Triggered",
								"status":  "False",
								"reason":  "Failed",
								"message": "out of memory",
							},
						},
					},
				},
			})
		}()
		return true, trigWatcher, nil
	})

	wrapper := newTestSandboxWrapper(agentsCS, dynCS, fakekube.NewSimpleClientset())

	resp := wrapper.Suspend(context.Background(), true, 5*time.Second)
	if resp.Success {
		t.Error("expected failure when snapshot creation fails")
	}
	if resp.SnapshotResponse == nil {
		t.Error("expected SnapshotResponse to be populated on failure")
	}
}

// ---------------------------------------------------------------------------
// Resume — error paths and snapshot restoration
// ---------------------------------------------------------------------------

func TestResume_IsSuspendedError(t *testing.T) {
	// No sandbox CR.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if resp.Success {
		t.Error("expected failure when sandbox CR does not exist")
	}
}

func TestResume_SetReplicasError(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)
	agentsCS.PrependReactor("patch", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("patch error")
	})
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if resp.Success {
		t.Error("expected failure when setReplicas returns error")
	}
}

func TestResume_FailsWhenListFails(t *testing.T) {
	// Sandbox is suspended (replicas=0) but has empty selector → List will fail
	// because resolveSandboxNameHash returns an error.
	sb := makeSandbox(0, "")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if resp.Success {
		t.Error("expected failure when snapshot list fails")
	}
	if resp.ErrorCode != ErrorCode {
		t.Errorf("expected ErrorCode, got %d", resp.ErrorCode)
	}
	if !strings.Contains(resp.ErrorReason, "failed to list snapshots") {
		t.Errorf("expected 'failed to list snapshots' in reason, got %q", resp.ErrorReason)
	}
}

func TestResume_WithSnapshotRestored(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)

	// Seed a ready snapshot so List returns it.
	snap := makeSnapshot("snap-uid-1", "h1", true)
	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			triggerGVR:  "PodSnapshotManualTriggerList",
			snapshotGVR: "PodSnapshotList",
		},
		snap,
	)

	// Pod is ready AND has PodRestored=True with correct UID.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: "PodRestored", Status: corev1.ConditionTrue, Message: "restored from snap-uid-1"},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	wrapper := newTestSandboxWrapper(agentsCS, dynCS, kubeCS)

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success, got: %s", resp.ErrorReason)
	}
	if !resp.RestoredFromSnapshot {
		t.Error("expected RestoredFromSnapshot=true")
	}
	if resp.SnapshotUID != "snap-uid-1" {
		t.Errorf("expected SnapshotUID=snap-uid-1, got %q", resp.SnapshotUID)
	}
}

func TestResume_WithSnapshotNotRestored(t *testing.T) {
	sb := makeSandbox(0, "agents.x-k8s.io/sandbox-name-hash=h1")
	agentsCS := makeAgentsClientset(sb)

	snap := makeSnapshot("snap-uid-1", "h1", true)
	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			triggerGVR:  "PodSnapshotManualTriggerList",
			snapshotGVR: "PodSnapshotList",
		},
		snap,
	)

	// Pod is ready but PodRestored condition is absent (fresh start).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	wrapper := newTestSandboxWrapper(agentsCS, dynCS, kubeCS)

	resp := wrapper.Resume(context.Background(), 5*time.Second)
	if resp.Success {
		t.Error("expected failure when restore check fails")
	}
	if resp.RestoredFromSnapshot {
		t.Error("expected RestoredFromSnapshot=false")
	}
}

// ---------------------------------------------------------------------------
// IsRestoredFromSnapshot — delegates to checkPodRestoredFromSnapshot
// ---------------------------------------------------------------------------

func TestIsRestoredFromSnapshot_Delegated(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: "PodRestored", Status: corev1.ConditionTrue, Message: "from snap-xyz"},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), kubeCS)

	result := wrapper.IsRestoredFromSnapshot(context.Background(), "snap-xyz")
	if !result.Success {
		t.Errorf("expected success: %s", result.ErrorReason)
	}
}

// ---------------------------------------------------------------------------
// Snapshots() — empty pod name propagates to engine
// ---------------------------------------------------------------------------

func TestSnapshots_EmptyPodName_PropagatesError(t *testing.T) {
	info := &stubInfo{sandboxName: "my-sandbox", podName: ""}
	handle := &stubHandle{}
	k8s := newTestK8sHelper(makeAgentsClientset(nil), newTestDynClient(), fakekube.NewSimpleClientset())
	wrapper := NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())

	resp := wrapper.Snapshots().Create(context.Background(), "test", 5*time.Second)
	if resp.Success {
		t.Error("expected failure when pod name is empty")
	}
}

// ---------------------------------------------------------------------------
// IsActive
// ---------------------------------------------------------------------------

func TestIsActive_FalseBeforeEngineInit(t *testing.T) {
	// A freshly constructed wrapper has no engine; IsActive must return false.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	if wrapper.IsActive() {
		t.Error("expected IsActive()=false before Snapshots() is called")
	}
}

func TestIsActive_TrueAfterEngineInit(t *testing.T) {
	// Calling Snapshots() lazily initialises the engine; IsActive must then return true.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	_ = wrapper.Snapshots() // triggers engine init
	if !wrapper.IsActive() {
		t.Error("expected IsActive()=true after Snapshots() is called")
	}
}

func TestIsActive_FalseAfterClose(t *testing.T) {
	// After Close() the underlying handle is no longer ready; IsActive must return false.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())
	_ = wrapper.Snapshots() // init engine so both conditions could be true

	if err := wrapper.Close(context.Background()); err != nil {
		t.Fatalf("Close returned unexpected error: %v", err)
	}
	if wrapper.IsActive() {
		t.Error("expected IsActive()=false after Close()")
	}
}

// ---------------------------------------------------------------------------
// Issue 3: resolveSandboxNameHash threads context correctly
// ---------------------------------------------------------------------------

// TestResolveSandboxNameHash_PropagatesNotFoundError verifies that resolveSandboxNameHash
// returns an error when the Sandbox CR does not exist in the cluster.
func TestResolveSandboxNameHash_PropagatesNotFoundError(t *testing.T) {
	// No sandbox CR — the API call returns not-found, which becomes an error.
	agentsCS := makeAgentsClientset(nil)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	_, err := wrapper.resolveSandboxNameHash(context.Background())
	if err == nil {
		t.Error("expected error when sandbox CR not found")
	}
}

func TestResolveSandboxNameHash_CacheHitSkipsAPICall(t *testing.T) {
	agentsCS := makeAgentsClientset(nil) // no sandbox in cluster
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	// Pre-populate the cache — no API call should happen.
	wrapper.snapshotHash = "pre-cached"

	hash, err := wrapper.resolveSandboxNameHash(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != "pre-cached" {
		t.Errorf("expected pre-cached, got %q", hash)
	}
}

// TestSuspend_CtxCancelledBeforeHashResolve verifies Suspend exits quickly when
// hash resolution fails (empty selector) regardless of ctx cancellation state.
func TestSuspend_CtxCancelledBeforeHashResolve(t *testing.T) {
	// Sandbox is running (replicas=1) with empty selector → resolveSandboxNameHash
	// returns an error immediately, causing Suspend to fail fast.
	sb := makeSandbox(1, "")
	agentsCS := makeAgentsClientset(sb)
	wrapper := newTestSandboxWrapper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	start := time.Now()
	resp := wrapper.Suspend(ctx, false, 10*time.Millisecond)
	if time.Since(start) > 2*time.Second {
		t.Error("Suspend blocked too long with a cancelled context")
	}
	// Suspend must fail: either hash resolution error or ctx-cancelled IsSuspended.
	if resp.Success {
		t.Error("expected Suspend to fail when hash cannot be resolved")
	}
}

func TestSuspend_EmptyPodName_SkipsTerminationWait(t *testing.T) {
	// Sandbox is running (replicas=1) with a valid selector, but the pod name is
	// not yet populated. Suspend should scale down and return success immediately
	// without making any pod API calls.
	sb := makeSandbox(1, "agents.x-k8s.io/sandbox-name-hash=hash1")
	agentsCS := makeAgentsClientset(sb)

	handle := &stubHandle{}
	info := &stubInfo{claimName: "my-claim", sandboxName: "my-sandbox", podName: ""}
	k8s := newTestK8sHelper(agentsCS, newTestDynClient(), fakekube.NewSimpleClientset())
	wrapper := NewSandboxWithSnapshotSupport(handle, info, k8s, "default", logr.Discard())

	resp := wrapper.Suspend(context.Background(), false, 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success when pod name is empty, got: %s", resp.ErrorReason)
	}
}
