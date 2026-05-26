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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestEngine(dynCS *fakedynamic.FakeDynamicClient, podName, hash string) *SnapshotEngine {
	return &SnapshotEngine{
		namespace: "default",
		dynClient: dynCS,
		log:       logr.Discard(),
		getPodName: func() (string, error) {
			return podName, nil
		},
		getSandboxNameHash: func() (string, error) {
			return hash, nil
		},
	}
}

func newDynClient(extra ...runtime.Object) *fakedynamic.FakeDynamicClient {
	return fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			triggerGVR:  "PodSnapshotManualTriggerList",
			snapshotGVR: "PodSnapshotList",
		},
		extra...,
	)
}

func makeSnapshot(name, hashLabel string, ready bool) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if ready {
		status["conditions"] = []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True"},
		}
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]interface{}{
				"name":              name,
				"namespace":         "default",
				"creationTimestamp": "2026-01-01T00:00:00Z",
				"labels":            map[string]interface{}{SandboxNameHashLabel: hashLabel},
			},
			"status": status,
		},
	}
}

// ---------------------------------------------------------------------------
// sanitizeTriggerName
// ---------------------------------------------------------------------------

func TestSanitizeTriggerName(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"simple", "my-trigger"},
		{"underscores", "my_trigger"},
		{"uppercase", "MY_TRIGGER"},
		{"empty", ""},
		{"long", "this-is-a-very-long-trigger-name-that-should-be-truncated-to-fit-k8s-limits"},
		{"all dashes", "---"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := sanitizeTriggerName(tc.input)
			if len(result) > 63 {
				t.Errorf("trigger name too long: %d chars: %s", len(result), result)
			}
			if result == "" {
				t.Error("trigger name must not be empty")
			}
			for _, ch := range result {
				if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-') {
					t.Errorf("invalid character %q in trigger name %q", ch, result)
				}
			}
		})
	}
}

// TestSanitizeTriggerName_BaseCapAt38 verifies the 38-char truncation rule.
// The suffix "-YYYYMMDD-HHMMSS-xxxxxxxx" is exactly 25 chars, so a base
// longer than 38 chars must be truncated to 38, yielding a 63-char total.
func TestSanitizeTriggerName_BaseCapAt38(t *testing.T) {
	// 50 lowercase letters — well over the 38-char base limit.
	longBase := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwx"
	result := sanitizeTriggerName(longBase)

	// Total must be exactly 63 chars: 38 (base) + 25 (suffix).
	if len(result) != 63 {
		t.Errorf("expected total length 63, got %d: %q", len(result), result)
	}

	// The base portion (everything before the first timestamp dash) must be
	// exactly 38 chars. The suffix format is "-YYYYMMDD-HHMMSS-xxxxxxxx",
	// so split on the first occurrence of "-2" (the timestamp prefix).
	// More robustly: count that the result starts with 38 chars of the base.
	if result[:38] != longBase[:38] {
		t.Errorf("base not correctly truncated to 38 chars: got prefix %q", result[:38])
	}
}

// TestSanitizeTriggerName_ExactlyAtLimit verifies that a base of exactly 38
// chars is kept as-is (not trimmed further).
func TestSanitizeTriggerName_ExactlyAtLimit(t *testing.T) {
	base38 := "abcdefghijklmnopqrstuvwxyzabcdefghijkl" // exactly 38 chars
	result := sanitizeTriggerName(base38)

	if len(result) != 63 {
		t.Errorf("expected total length 63, got %d: %q", len(result), result)
	}
	if result[:38] != base38 {
		t.Errorf("38-char base was unexpectedly modified: got prefix %q", result[:38])
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.List
// ---------------------------------------------------------------------------

func TestSnapshotEngine_List_AllReady(t *testing.T) {
	snap1 := makeSnapshot("snap-1", "abc123", true)
	snap2 := makeSnapshot("snap-2", "abc123", false)
	dynCS := newDynClient(snap1, snap2)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: true})

	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.ErrorReason)
	}
	if len(result.Snapshots) != 1 {
		t.Fatalf("expected 1 ready snapshot, got %d", len(result.Snapshots))
	}
	if result.Snapshots[0].SnapshotUID != "snap-1" {
		t.Errorf("expected snap-1, got %s", result.Snapshots[0].SnapshotUID)
	}
}

func TestSnapshotEngine_List_IncludeNotReady(t *testing.T) {
	snap1 := makeSnapshot("snap-1", "abc123", true)
	snap2 := makeSnapshot("snap-2", "abc123", false)
	dynCS := newDynClient(snap1, snap2)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: false})

	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.ErrorReason)
	}
	if len(result.Snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(result.Snapshots))
	}
}

func TestSnapshotEngine_List_NoPodName(t *testing.T) {
	dynCS := newDynClient()
	eng := &SnapshotEngine{
		namespace:  "default",
		dynClient:  dynCS,
		log:        logr.Discard(),
		getPodName: func() (string, error) { return "", nil },
		getSandboxNameHash: func() (string, error) {
			return "abc123", nil
		},
	}

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: true})
	if result.Success {
		t.Error("expected failure when pod name is empty")
	}
}

func TestSnapshotEngine_List_NoHash(t *testing.T) {
	dynCS := newDynClient()
	eng := &SnapshotEngine{
		namespace:          "default",
		dynClient:          dynCS,
		log:                logr.Discard(),
		getPodName:         func() (string, error) { return "my-pod", nil },
		getSandboxNameHash: func() (string, error) { return "", nil },
	}

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: true})
	if result.Success {
		t.Error("expected failure when sandbox name hash is empty")
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.Delete
// ---------------------------------------------------------------------------

func TestSnapshotEngine_Delete_Success(t *testing.T) {
	snap := makeSnapshot("snap-1", "abc123", true)
	dynCS := newDynClient(snap)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// Inject a watch that immediately returns DELETED.
	snapWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshots", func(action ktesting.Action) (bool, watch.Interface, error) {
		go func() {
			snapWatcher.Delete(snap)
		}()
		return true, snapWatcher, nil
	})

	result := eng.Delete(context.Background(), "snap-1", 5*time.Second)
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorReason)
	}
	if len(result.DeletedSnapshots) != 1 || result.DeletedSnapshots[0] != "snap-1" {
		t.Errorf("unexpected deleted snapshots: %v", result.DeletedSnapshots)
	}
}

func TestSnapshotEngine_Delete_AlreadyGone(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// Object doesn't exist; waitForSnapshotDeletion first GETs the object to check.
	// The GET will return 404, so deletion is considered successful immediately.
	result := eng.Delete(context.Background(), "nonexistent", 5*time.Second)
	if !result.Success {
		t.Fatalf("expected success for already-deleted snapshot, got: %s", result.ErrorReason)
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.DeleteManualTriggers
// ---------------------------------------------------------------------------

func TestSnapshotEngine_DeleteManualTriggers(t *testing.T) {
	trigger := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
			"kind":       PodSnapshotTriggerKind,
			"metadata": map[string]interface{}{
				"name":      "my-trigger",
				"namespace": "default",
			},
		},
	}
	dynCS := newDynClient(trigger)
	eng := newTestEngine(dynCS, "my-pod", "abc123")
	eng.createdManualTriggers = []string{"my-trigger"}

	eng.DeleteManualTriggers(context.Background())

	if len(eng.createdManualTriggers) != 0 {
		t.Errorf("expected createdManualTriggers to be empty, got %v", eng.createdManualTriggers)
	}
}

func TestSnapshotEngine_DeleteManualTriggers_AlreadyGone(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")
	eng.createdManualTriggers = []string{"ghost-trigger"}

	eng.DeleteManualTriggers(context.Background())

	// A 404 is silently ignored; no remaining triggers.
	if len(eng.createdManualTriggers) != 0 {
		t.Errorf("expected empty triggers after 404, got %v", eng.createdManualTriggers)
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.Create (watch-based)
// ---------------------------------------------------------------------------

func TestSnapshotEngine_Create_Success(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// Inject a watch reactor that immediately fires MODIFIED with completion status.
	trigWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshotmanualtriggers", func(action ktesting.Action) (bool, watch.Interface, error) {
		go func() {
			completedTrigger := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
					"kind":       PodSnapshotTriggerKind,
					"metadata": map[string]interface{}{
						"name":      "snap",
						"namespace": "default",
					},
					"status": map[string]interface{}{
						"snapshotCreated": map[string]interface{}{"name": "snap-uid-123"},
						"conditions": []interface{}{
							map[string]interface{}{
								"type":               "Triggered",
								"status":             "True",
								"reason":             "Complete",
								"lastTransitionTime": "2026-01-01T00:00:00Z",
							},
						},
					},
				},
			}
			trigWatcher.Modify(completedTrigger)
		}()
		return true, trigWatcher, nil
	})

	resp := eng.Create(context.Background(), "test-snapshot", 5*time.Second)
	if !resp.Success {
		t.Fatalf("expected success, got: %s", resp.ErrorReason)
	}
	if resp.SnapshotUID != "snap-uid-123" {
		t.Errorf("expected snapshotUID=snap-uid-123, got %q", resp.SnapshotUID)
	}
	if len(eng.createdManualTriggers) != 1 {
		t.Errorf("expected 1 tracked trigger, got %d", len(eng.createdManualTriggers))
	}
}

func TestSnapshotEngine_Create_Failure(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	trigWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshotmanualtriggers", func(action ktesting.Action) (bool, watch.Interface, error) {
		go func() {
			failedTrigger := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
					"kind":       PodSnapshotTriggerKind,
					"metadata": map[string]interface{}{"name": "snap", "namespace": "default"},
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":    "Triggered",
								"status":  "False",
								"reason":  "Failed",
								"message": "out of disk space",
							},
						},
					},
				},
			}
			trigWatcher.Modify(failedTrigger)
		}()
		return true, trigWatcher, nil
	})

	resp := eng.Create(context.Background(), "test-snapshot", 5*time.Second)
	if resp.Success {
		t.Error("expected failure for failed snapshot")
	}
}

func TestSnapshotEngine_Create_Timeout(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// Return a watcher that never fires.
	dynCS.PrependWatchReactor("podsnapshotmanualtriggers", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewFake(), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp := eng.Create(ctx, "test-snapshot", 5*time.Second)
	if resp.Success {
		t.Error("expected failure on timeout")
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.DeleteAll
// ---------------------------------------------------------------------------

func TestSnapshotEngine_DeleteAll_Empty(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result, err := eng.DeleteAll(context.Background(), "all", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorReason)
	}
	if len(result.DeletedSnapshots) != 0 {
		t.Errorf("expected no deletions, got %v", result.DeletedSnapshots)
	}
}

func TestSnapshotEngine_DeleteAll_InvalidStrategy(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	_, err := eng.DeleteAll(context.Background(), "unknown", nil, 5*time.Second)
	if err == nil {
		t.Error("expected error for unknown deleteBy strategy")
	}
}

func TestSnapshotEngine_DeleteAll_ByLabelsRequiresLabels(t *testing.T) {
	dynCS := newDynClient()
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	_, err := eng.DeleteAll(context.Background(), "labels", nil, 5*time.Second)
	if err == nil {
		t.Error("expected error when deleteBy=labels but no labelValue")
	}

	// Provide metav1 timestamp import check.
	_ = metav1.Now()
}

// ---------------------------------------------------------------------------
// SnapshotEngine.Create — error paths
// ---------------------------------------------------------------------------

func TestSnapshotEngine_Create_EmptyPodName(t *testing.T) {
	dynCS := newDynClient()
	eng := &SnapshotEngine{
		namespace:          "default",
		dynClient:          dynCS,
		log:                logr.Discard(),
		getPodName:         func() (string, error) { return "", nil },
		getSandboxNameHash: func() (string, error) { return "abc123", nil },
	}

	resp := eng.Create(context.Background(), "test", 5*time.Second)
	if resp.Success {
		t.Error("expected failure when pod name is empty")
	}
}

func TestSnapshotEngine_Create_APIError(t *testing.T) {
	dynCS := newDynClient()
	dynCS.PrependReactor("create", "podsnapshotmanualtriggers", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("server error")
	})
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	resp := eng.Create(context.Background(), "test", 5*time.Second)
	if resp.Success {
		t.Error("expected failure on API error creating trigger")
	}
	// Trigger name should still be returned for debugging.
	if resp.TriggerName == "" {
		t.Error("TriggerName should be set even on failure")
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.List — additional paths
// ---------------------------------------------------------------------------

func TestSnapshotEngine_List_WithGroupingLabels(t *testing.T) {
	snap := makeSnapshot("snap-1", "abc123", true)
	dynCS := newDynClient(snap)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.List(context.Background(), SnapshotFilter{
		ReadyOnly:      true,
		GroupingLabels: map[string]string{"env": "prod"},
	})

	// The fake dynamic client ignores label selectors, so we just verify the
	// call succeeds and returns structurally valid results.
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorReason)
	}
}

func TestSnapshotEngine_List_APIError(t *testing.T) {
	dynCS := newDynClient()
	dynCS.PrependReactor("list", "podsnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("network error")
	})
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: false})
	if result.Success {
		t.Error("expected failure on list API error")
	}
}

func TestSnapshotEngine_List_WithPodAnnotation(t *testing.T) {
	snap := makeSnapshot("snap-1", "abc123", true)
	// Add the pod annotation.
	annotations := map[string]interface{}{PodSnapshotPodAnnotation: "origin-pod-xyz"}
	snap.Object["metadata"].(map[string]interface{})["annotations"] = annotations

	dynCS := newDynClient(snap)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.List(context.Background(), SnapshotFilter{ReadyOnly: false})
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorReason)
	}
	if len(result.Snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(result.Snapshots))
	}
	if result.Snapshots[0].SourcePod != "origin-pod-xyz" {
		t.Errorf("expected SourcePod=origin-pod-xyz, got %q", result.Snapshots[0].SourcePod)
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.DeleteAll — "labels" path
// ---------------------------------------------------------------------------

func TestSnapshotEngine_DeleteAll_ByLabels_Success(t *testing.T) {
	snap := makeSnapshot("snap-1", "abc123", true)
	dynCS := newDynClient(snap)
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// Inject DELETED event so waitForSnapshotDeletion sees it gone immediately.
	snapWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshots", func(action ktesting.Action) (bool, watch.Interface, error) {
		go func() { snapWatcher.Delete(snap) }()
		return true, snapWatcher, nil
	})

	result, err := eng.DeleteAll(context.Background(), "labels", map[string]string{"env": "test"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.ErrorReason)
	}
}

// ---------------------------------------------------------------------------
// SnapshotEngine.DeleteManualTriggers — retry/leak paths
// ---------------------------------------------------------------------------

func TestSnapshotEngine_DeleteManualTriggers_RetryAndLeak(t *testing.T) {
	dynCS := newDynClient()
	// Always fail with a non-404 error so retries are exhausted.
	dynCS.PrependReactor("delete", "podsnapshotmanualtriggers", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("server unavailable")
	})
	eng := newTestEngine(dynCS, "my-pod", "abc123")
	eng.createdManualTriggers = []string{"trigger-1"}

	eng.DeleteManualTriggers(context.Background())

	// After maxRetries exhausted, trigger should be reported as leaked.
	if len(eng.createdManualTriggers) == 0 {
		t.Error("expected trigger to remain in createdManualTriggers after exhausting retries")
	}
}

// ---------------------------------------------------------------------------
// executeDeletion — delete API error and partial failure
// ---------------------------------------------------------------------------

func TestSnapshotEngine_executeDeletion_DeleteAPIError(t *testing.T) {
	snap := makeSnapshot("snap-1", "abc123", true)
	dynCS := newDynClient(snap)
	// Fail delete with a non-404 error.
	dynCS.PrependReactor("delete", "podsnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden")
	})
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	result := eng.executeDeletion(context.Background(), "snap-1", "", nil, 5*time.Second)
	if result.Success {
		t.Error("expected failure when delete API returns error")
	}
	if result.ErrorReason == "" {
		t.Error("ErrorReason should be set")
	}
}

func TestSnapshotEngine_executeDeletion_PartialFailure(t *testing.T) {
	snap1 := makeSnapshot("snap-1", "abc123", true)
	snap2 := makeSnapshot("snap-2", "abc123", true)
	dynCS := newDynClient(snap1, snap2)

	// snap-1 deletes successfully; snap-2 gets a DELETED event.
	// snap-1: let the first delete succeed; snap-2: return an error.
	callCount := 0
	dynCS.PrependReactor("delete", "podsnapshots", func(action ktesting.Action) (bool, runtime.Object, error) {
		callCount++
		if callCount == 1 {
			return true, nil, fmt.Errorf("network error for snap-1")
		}
		return false, nil, nil // let snap-2 go through default handling
	})
	eng := newTestEngine(dynCS, "my-pod", "abc123")

	// For snap-2: inject a DELETED watch event.
	snapWatcher := watch.NewFake()
	dynCS.PrependWatchReactor("podsnapshots", func(action ktesting.Action) (bool, watch.Interface, error) {
		go func() { snapWatcher.Delete(snap2) }()
		return true, snapWatcher, nil
	})

	result := eng.executeDeletion(context.Background(), "", "global", nil, 5*time.Second)
	// At least one error and at least one deletion: partial failure.
	if result.Success {
		t.Error("expected partial failure result")
	}
	if len(result.DeletedSnapshots) == 0 {
		t.Error("expected at least one successfully deleted snapshot in partial failure")
	}
}
