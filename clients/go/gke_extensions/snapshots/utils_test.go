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
)

// ---------------------------------------------------------------------------
// checkPodRestoredFromSnapshot
// ---------------------------------------------------------------------------

func makePod(conditions []corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: conditions,
		},
	}
}

func TestCheckPodRestoredFromSnapshot_Success(t *testing.T) {
	snapshotUID := "snap-123"
	pod := makePod([]corev1.PodCondition{
		{
			Type:    "PodRestored",
			Status:  corev1.ConditionTrue,
			Message: "restored from snapshot snap-123",
		},
	})
	kubeCS := fakekube.NewSimpleClientset(pod)

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		snapshotUID,
		logr.Discard(),
	)

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.ErrorReason)
	}
}

func TestCheckPodRestoredFromSnapshot_WrongUID(t *testing.T) {
	pod := makePod([]corev1.PodCondition{
		{
			Type:    "PodRestored",
			Status:  corev1.ConditionTrue,
			Message: "restored from snapshot other-uid",
		},
	})
	kubeCS := fakekube.NewSimpleClientset(pod)

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"snap-123",
		logr.Discard(),
	)

	if result.Success {
		t.Error("expected failure when UID does not match")
	}
}

func TestCheckPodRestoredFromSnapshot_NoCondition(t *testing.T) {
	pod := makePod(nil)
	kubeCS := fakekube.NewSimpleClientset(pod)

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"snap-123",
		logr.Discard(),
	)

	if result.Success {
		t.Error("expected failure with no conditions")
	}
}

func TestCheckPodRestoredFromSnapshot_FreshStart(t *testing.T) {
	pod := makePod([]corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	})
	kubeCS := fakekube.NewSimpleClientset(pod)

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"snap-123",
		logr.Discard(),
	)

	if result.Success {
		t.Error("expected failure (fresh start, no PodRestored condition)")
	}
}

func TestCheckPodRestoredFromSnapshot_PodNotFound(t *testing.T) {
	kubeCS := fakekube.NewSimpleClientset()

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"nonexistent",
		"snap-123",
		logr.Discard(),
	)

	if result.Success {
		t.Error("expected failure when pod not found")
	}
}

func TestCheckPodRestoredFromSnapshot_RestoreFailed(t *testing.T) {
	pod := makePod([]corev1.PodCondition{
		{
			Type:    "PodRestored",
			Status:  corev1.ConditionFalse,
			Reason:  "RestoreFailed",
			Message: "snapshot not found",
		},
	})
	kubeCS := fakekube.NewSimpleClientset(pod)

	result := checkPodRestoredFromSnapshot(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"snap-123",
		logr.Discard(),
	)

	if result.Success {
		t.Error("expected failure when restore failed")
	}
}

// ---------------------------------------------------------------------------
// waitForPodTermination
// ---------------------------------------------------------------------------

func TestWaitForPodTermination_AlreadyGone(t *testing.T) {
	kubeCS := fakekube.NewSimpleClientset()

	// Pod doesn't exist — should return true immediately.
	done := waitForPodTermination(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"gone-pod",
		"uid-1",
		5*time.Second,
		logr.Discard(),
	)
	if !done {
		t.Error("expected true when pod is not found")
	}
}

func TestWaitForPodTermination_UIDChanged(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
			UID:       "new-uid",
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	// Old UID is "old-uid"; current pod has "new-uid" → termination detected.
	done := waitForPodTermination(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"old-uid",
		5*time.Second,
		logr.Discard(),
	)
	if !done {
		t.Error("expected true when pod UID changed")
	}
}

func TestWaitForPodTermination_Timeout(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
			UID:       "same-uid",
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := waitForPodTermination(
		ctx,
		kubeCS.CoreV1(),
		"default",
		"my-pod",
		"same-uid",
		5*time.Second,
		logr.Discard(),
	)
	if done {
		t.Error("expected false on timeout")
	}
}

// ---------------------------------------------------------------------------
// waitForPodReady
// ---------------------------------------------------------------------------

func TestWaitForPodReady_Ready(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	done := waitForPodReady(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		func() string { return "my-pod" },
		5*time.Second,
		logr.Discard(),
	)
	if !done {
		t.Error("expected true for ready pod")
	}
}

func TestWaitForPodReady_Timeout(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := waitForPodReady(
		ctx,
		kubeCS.CoreV1(),
		"default",
		func() string { return "my-pod" },
		5*time.Second,
		logr.Discard(),
	)
	if done {
		t.Error("expected false on timeout when pod is not ready")
	}
}

func TestWaitForPodReady_SkipsTerminating(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-pod",
			Namespace:         "default",
			DeletionTimestamp: &now,
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := waitForPodReady(
		ctx,
		kubeCS.CoreV1(),
		"default",
		func() string { return "my-pod" },
		5*time.Second,
		logr.Discard(),
	)
	if done {
		t.Error("expected false for terminating pod even if conditions say Ready")
	}
}

func TestWaitForPodReady_EmptyNameEventuallyPopulates(t *testing.T) {
	// Simulates the case where pod name is not yet known immediately after scale-up.
	// The first call to getPodName returns ""; subsequent calls return "my-pod".
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)

	callCount := 0
	getPodName := func() string {
		callCount++
		if callCount == 1 {
			return "" // first iteration: name not yet assigned
		}
		return "my-pod"
	}

	done := waitForPodReady(
		context.Background(),
		kubeCS.CoreV1(),
		"default",
		getPodName,
		10*time.Second,
		logr.Discard(),
	)
	if !done {
		t.Error("expected true when pod name eventually becomes available")
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 getPodName calls, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// extractSnapshotResult
// ---------------------------------------------------------------------------

func TestExtractSnapshotResult_Complete(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"snapshotCreated": map[string]any{"name": "snap-abc"},
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
	}
	result, err := extractSnapshotResult(obj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SnapshotUID != "snap-abc" {
		t.Errorf("expected snap-abc, got %s", result.SnapshotUID)
	}
}

func TestExtractSnapshotResult_Failed(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":    "Triggered",
						"status":  "False",
						"reason":  "Failed",
						"message": "disk full",
					},
				},
			},
		},
	}
	_, err := extractSnapshotResult(obj)
	if err == nil || err == errNotYetComplete {
		t.Error("expected non-nil error for failed snapshot")
	}
}

func TestExtractSnapshotResult_NotYetComplete(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "Triggered",
						"status": "False",
						"reason": "InProgress",
					},
				},
			},
		},
	}
	_, err := extractSnapshotResult(obj)
	if err != errNotYetComplete {
		t.Errorf("expected errNotYetComplete, got %v", err)
	}
}

func init() {
	// Force the compiler to keep the runtime import used only for test helpers.
	_ = runtime.NewScheme()
}

// ---------------------------------------------------------------------------
// drainDeletionWatch
// ---------------------------------------------------------------------------

func TestDrainDeletionWatch_Deleted(t *testing.T) {
	watcher := watch.NewFake()
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "snap-1"}}}
	go func() { watcher.Delete(obj) }()

	done, err := drainDeletionWatch(context.Background(), watcher, "snap-1", logr.Discard())
	if !done || err != nil {
		t.Errorf("expected (true, nil), got (%v, %v)", done, err)
	}
}

func TestDrainDeletionWatch_WatchError(t *testing.T) {
	watcher := watch.NewFake()
	go func() {
		watcher.Error(&metav1.Status{Status: "Failure"})
		watcher.Stop()
	}()

	done, err := drainDeletionWatch(context.Background(), watcher, "snap-1", logr.Discard())
	// watch.Error causes re-list (done=false, err=nil).
	if done || err != nil {
		t.Errorf("expected (false, nil) on watch.Error, got (%v, %v)", done, err)
	}
}

func TestDrainDeletionWatch_ChannelClosed(t *testing.T) {
	watcher := watch.NewFake()
	go func() { watcher.Stop() }()

	done, err := drainDeletionWatch(context.Background(), watcher, "snap-1", logr.Discard())
	if done || err != nil {
		t.Errorf("expected (false, nil) on closed channel, got (%v, %v)", done, err)
	}
}

func TestDrainDeletionWatch_ContextCancelled(t *testing.T) {
	watcher := watch.NewFake() // never fires
	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()

	_, err := drainDeletionWatch(ctx, watcher, "snap-1", logr.Discard())
	if err == nil {
		t.Error("expected error when context is cancelled")
	}
}

// ---------------------------------------------------------------------------
// waitForSnapshotDeletion — watch path (snapshot exists, then DELETED)
// ---------------------------------------------------------------------------

func TestWaitForSnapshotDeletion_WatchPath(t *testing.T) {
	snap := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
			"kind":       "PodSnapshot",
			"metadata":   map[string]any{"name": "snap-1", "namespace": "default"},
		},
	}
	dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{snapshotGVR: "PodSnapshotList"},
		snap,
	)

	snapWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshots", func(_ ktesting.Action) (bool, watch.Interface, error) {
		go func() { snapWatcher.Delete(snap) }()
		return true, snapWatcher, nil
	})

	err := waitForSnapshotDeletion(context.Background(), dynCS, "default", "snap-1", 5*time.Second, logr.Discard())
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// drainTriggerWatch — edge cases
// ---------------------------------------------------------------------------

func TestDrainTriggerWatch_Deleted(t *testing.T) {
	watcher := watch.NewFake()
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "t"}}}
	go func() { watcher.Delete(obj) }()

	_, done, err := drainTriggerWatch(context.Background(), watcher, "t", logr.Discard())
	if done {
		t.Error("expected done=false for deleted trigger")
	}
	if err == nil {
		t.Error("expected error (ErrSnapshotFailed) when trigger is deleted")
	}
}

func TestDrainTriggerWatch_WatchError(t *testing.T) {
	watcher := watch.NewFake()
	go func() {
		watcher.Error(&metav1.Status{Status: "Failure"})
		watcher.Stop()
	}()

	_, done, err := drainTriggerWatch(context.Background(), watcher, "t", logr.Discard())
	// watch.Error → re-list (done=false, err=nil).
	if done || err != nil {
		t.Errorf("expected (zero, false, nil) on watch.Error, got (%v, %v)", done, err)
	}
}

func TestDrainTriggerWatch_ChannelClosed(t *testing.T) {
	watcher := watch.NewFake()
	go func() { watcher.Stop() }()

	_, done, err := drainTriggerWatch(context.Background(), watcher, "t", logr.Discard())
	if done || err != nil {
		t.Errorf("expected (zero, false, nil) on closed channel, got (%v, %v)", done, err)
	}
}

// ---------------------------------------------------------------------------
// waitForPodTermination — non-404 API error is logged and loop continues
// ---------------------------------------------------------------------------

func TestWaitForPodTermination_APIError_ThenGone(t *testing.T) {
	// First call returns a server error; second call returns 404 → terminated.
	callCount := 0
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default", UID: "uid-old"},
	}
	kubeCS := fakekube.NewSimpleClientset(pod)
	kubeCS.PrependReactor("get", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		callCount++
		if callCount == 1 {
			return true, nil, fmt.Errorf("server error")
		}
		return false, nil, nil // let default (404 after deletion) handle it
	})
	// Delete the pod so the second (passthrough) call returns 404.
	kubeCS.CoreV1().Pods("default").Delete(context.Background(), "my-pod", metav1.DeleteOptions{}) //nolint:errcheck

	done := waitForPodTermination(
		context.Background(), kubeCS.CoreV1(), "default", "my-pod", "uid-old",
		5*time.Second, logr.Discard(),
	)
	if !done {
		t.Error("expected true when pod eventually disappears after an API error")
	}
}

// ---------------------------------------------------------------------------
// Issue 7: errNotYetComplete uses errors.New (sentinel comparability)
// ---------------------------------------------------------------------------

func TestErrNotYetComplete_IsComparable(t *testing.T) {
	if !errors.Is(errNotYetComplete, errNotYetComplete) {
		t.Error("errNotYetComplete must be comparable to itself via errors.Is")
	}
}

func TestErrNotYetComplete_IsNotWrapped(t *testing.T) {
	if errors.Unwrap(errNotYetComplete) != nil {
		t.Error("errNotYetComplete must not wrap another error (should use errors.New)")
	}
}

func TestErrNotYetComplete_NotConfusedWithOtherErrors(t *testing.T) {
	other := errors.New("snapshot not yet complete") // different instance
	if errors.Is(errNotYetComplete, other) {
		t.Error("errNotYetComplete must not match a different error with the same message")
	}
}
