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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

var (
	triggerGVR = schema.GroupVersionResource{
		Group:    PodSnapshotAPIGroup,
		Version:  PodSnapshotAPIVersion,
		Resource: PodSnapshotTriggerPlural,
	}
	snapshotGVR = schema.GroupVersionResource{
		Group:    PodSnapshotAPIGroup,
		Version:  PodSnapshotAPIVersion,
		Resource: PodSnapshotPlural,
	}
)

// waitForSnapshotCompleted watches the PodSnapshotManualTrigger until it
// reports completion (Triggered=True/Complete) or failure.
func waitForSnapshotCompleted(
	ctx context.Context,
	dynClient dynamic.Interface,
	namespace, triggerName, resourceVersion string,
	timeout time.Duration,
	log logr.Logger,
) (snapshotResult, error) {
	log.Info("waiting for snapshot manual trigger to complete", "trigger", triggerName)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	listOpts := metav1.ListOptions{
		FieldSelector: "metadata.name=" + triggerName,
	}
	if resourceVersion != "" {
		listOpts.ResourceVersion = resourceVersion
	}

	for {
		watcher, err := dynClient.Resource(triggerGVR).Namespace(namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				return snapshotResult{}, fmt.Errorf("%w: trigger %s: %w", ErrSnapshotTimeout, triggerName, ctx.Err())
			}
			return snapshotResult{}, fmt.Errorf("watching snapshot trigger %s: %w", triggerName, err)
		}

		result, done, watchErr := drainTriggerWatch(ctx, watcher, triggerName, log)
		watcher.Stop()

		if done {
			return result, nil
		}
		if watchErr != nil {
			return snapshotResult{}, watchErr
		}
		// Watch channel closed; re-establish.
		log.V(1).Info("trigger watch closed, re-establishing", "trigger", triggerName)
	}
}

func drainTriggerWatch(ctx context.Context, watcher watch.Interface, triggerName string, log logr.Logger) (snapshotResult, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return snapshotResult{}, false, fmt.Errorf("%w: trigger %s", ErrSnapshotTimeout, triggerName)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return snapshotResult{}, false, nil
			}
			switch event.Type {
			case watch.Error:
				log.V(1).Info("transient trigger watch error, re-listing", "error", event.Object)
				return snapshotResult{}, false, nil
			case watch.Deleted:
				return snapshotResult{}, false, fmt.Errorf("%w: trigger %s was deleted before completion", ErrSnapshotFailed, triggerName)
			case watch.Added, watch.Modified:
				obj, ok := event.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				result, err := extractSnapshotResult(obj)
				if err == errNotYetComplete {
					continue
				}
				if err != nil {
					return snapshotResult{}, false, fmt.Errorf("%w: %w", ErrSnapshotFailed, err)
				}
				log.Info("snapshot trigger completed", "trigger", triggerName, "snapshotUID", result.SnapshotUID)
				return result, true, nil
			}
		}
	}
}

// errNotYetComplete is a sentinel returned by extractSnapshotResult when the
// trigger has not yet reached a terminal state.
var errNotYetComplete = errors.New("snapshot not yet complete")

func extractSnapshotResult(obj *unstructured.Unstructured) (snapshotResult, error) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		reason, _ := cond["reason"].(string)

		if typ != "Triggered" {
			continue
		}
		if status == "True" && reason == "Complete" {
			uid, _, _ := unstructured.NestedString(obj.Object, "status", "snapshotCreated", "name")
			tsStr, _ := cond["lastTransitionTime"].(string)
			var ts time.Time
			if tsStr != "" {
				ts, _ = time.Parse(time.RFC3339, tsStr)
			}
			return snapshotResult{SnapshotUID: uid, SnapshotTimestamp: ts}, nil
		}
		if status == "False" && (reason == "Failed" || reason == "Error") {
			msg, _ := cond["message"].(string)
			return snapshotResult{}, fmt.Errorf("snapshot failed: %s", msg)
		}
	}
	return snapshotResult{}, errNotYetComplete
}

// checkPodRestoredFromSnapshot reads the pod and verifies the PodRestored condition.
func checkPodRestoredFromSnapshot(
	ctx context.Context,
	coreClient corev1client.CoreV1Interface,
	namespace, podName, snapshotUID string,
	log logr.Logger,
) RestoreCheckResult {
	pod, err := coreClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		log.Error(err, "failed to check pod restore status", "pod", podName)
		return RestoreCheckResult{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to read pod: %v", err),
			ErrorCode:   ErrorCode,
		}
	}

	if pod.Status.Conditions == nil {
		return RestoreCheckResult{
			Success:     false,
			ErrorReason: "pod status or conditions not found",
			ErrorCode:   ErrorCode,
		}
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type != "PodRestored" {
			continue
		}
		if cond.Status == corev1.ConditionTrue {
			if strings.Contains(cond.Message, snapshotUID) {
				return RestoreCheckResult{Success: true, ErrorCode: SuccessCode}
			}
			return RestoreCheckResult{
				Success:     false,
				ErrorReason: fmt.Sprintf("pod was not restored from snapshot %q; actual condition message: %q", snapshotUID, cond.Message),
				ErrorCode:   ErrorCode,
			}
		}
		return RestoreCheckResult{
			Success:     false,
			ErrorReason: fmt.Sprintf("restore attempted but pending or failed (status: %q, reason: %q, message: %q)", cond.Status, cond.Reason, cond.Message),
			ErrorCode:   ErrorCode,
		}
	}

	return RestoreCheckResult{
		Success:     false,
		ErrorReason: "pod was started as a fresh instance (no PodRestored condition found)",
		ErrorCode:   ErrorCode,
	}
}

// waitForSnapshotDeletion watches until the PodSnapshot is gone.
func waitForSnapshotDeletion(
	ctx context.Context,
	dynClient dynamic.Interface,
	namespace, snapshotUID string,
	timeout time.Duration,
	log logr.Logger,
) error {
	// Quick check: already deleted?
	_, err := dynClient.Resource(snapshotGVR).Namespace(namespace).Get(ctx, snapshotUID, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			log.V(1).Info("snapshot already deleted", "uid", snapshotUID)
			return nil
		}
		return fmt.Errorf("checking snapshot existence: %w", err)
	}

	log.Info("waiting for snapshot deletion", "uid", snapshotUID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	listOpts := metav1.ListOptions{FieldSelector: "metadata.name=" + snapshotUID}

	for {
		watcher, err := dynClient.Resource(snapshotGVR).Namespace(namespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("timed out waiting for snapshot %s deletion", snapshotUID)
			}
			return fmt.Errorf("watching snapshot %s deletion: %w", snapshotUID, err)
		}

		done, watchErr := drainDeletionWatch(ctx, watcher, snapshotUID, log)
		watcher.Stop()

		if done {
			return nil
		}
		if watchErr != nil {
			return watchErr
		}
		log.V(1).Info("snapshot deletion watch closed, re-establishing", "uid", snapshotUID)
	}
}

func drainDeletionWatch(ctx context.Context, watcher watch.Interface, snapshotUID string, log logr.Logger) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("timed out waiting for snapshot %s deletion", snapshotUID)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return false, nil
			}
			switch event.Type {
			case watch.Deleted:
				log.Info("snapshot deletion confirmed", "uid", snapshotUID)
				return true, nil
			case watch.Error:
				log.V(1).Info("transient deletion watch error, re-listing", "error", event.Object)
				return false, nil
			}
		}
	}
}

// waitForPodTermination polls until the pod is gone or its UID changes.
// Returns true if the pod terminated within the timeout.
func waitForPodTermination(
	ctx context.Context,
	coreClient corev1client.CoreV1Interface,
	namespace, podName, podUID string,
	timeout time.Duration,
	log logr.Logger,
) bool {
	log.Info("waiting for pod termination", "pod", podName, "uid", podUID, "timeout", timeout)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pod, err := coreClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return true
			}
			log.Error(err, "error checking pod status during termination wait", "pod", podName)
		} else if string(pod.UID) != podUID {
			// A new pod took the name; old one has terminated.
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}

	log.Info("timed out waiting for pod termination", "pod", podName)
	return false
}

// waitForPodReady polls until the pod has Ready=True.
// Returns true if the pod became ready within the timeout.
func waitForPodReady(
	ctx context.Context,
	coreClient corev1client.CoreV1Interface,
	namespace, podName string,
	timeout time.Duration,
	log logr.Logger,
) bool {
	log.Info("waiting for pod to become ready", "pod", podName, "timeout", timeout)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pod, err := coreClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil && pod.DeletionTimestamp == nil {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}

	log.Info("timed out waiting for pod to become ready", "pod", podName)
	return false
}
