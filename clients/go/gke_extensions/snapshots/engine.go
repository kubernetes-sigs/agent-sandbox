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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	sandbox "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// SnapshotEngine manages PodSnapshot CRUD operations for a sandbox.
type SnapshotEngine struct {
	namespace          string
	dynClient          dynamic.Interface
	getPodName         func(ctx context.Context) (string, error)
	getSandboxNameHash func(ctx context.Context) (string, error)
	log                logr.Logger

	mu                    sync.Mutex
	createdManualTriggers []string
}

// NewSnapshotEngine creates a SnapshotEngine backed by the provided K8sHelper.
// getPodName and getSandboxNameHash are callbacks that resolve dynamic identity
// values; ctx is forwarded from each engine operation to the callbacks.
func NewSnapshotEngine(
	namespace string,
	k8s *sandbox.K8sHelper,
	getPodName func(ctx context.Context) (string, error),
	getSandboxNameHash func(ctx context.Context) (string, error),
	log logr.Logger,
) *SnapshotEngine {
	return &SnapshotEngine{
		namespace:          namespace,
		dynClient:          k8s.DynamicClient,
		getPodName:         getPodName,
		getSandboxNameHash: getSandboxNameHash,
		log:                log,
	}
}

// Create takes a snapshot of the sandbox pod. triggerName is sanitised and
// made unique; timeout controls how long to wait for the snapshot to complete.
func (e *SnapshotEngine) Create(ctx context.Context, triggerName string, timeout time.Duration) SnapshotResponse {
	safeName := sanitizeTriggerName(triggerName)

	podName, err := e.getPodName(ctx)
	if err != nil || podName == "" {
		return SnapshotResponse{
			Success:     false,
			TriggerName: safeName,
			ErrorReason: fmt.Sprintf("pod name not available: %v", err),
			ErrorCode:   ErrorCode,
		}
	}

	manifest := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion,
			"kind":       PodSnapshotTriggerKind,
			"metadata": map[string]any{
				"name":      safeName,
				"namespace": e.namespace,
			},
			"spec": map[string]any{
				"targetPod": podName,
			},
		},
	}

	created, err := e.dynClient.Resource(triggerGVR).Namespace(e.namespace).Create(ctx, manifest, metav1.CreateOptions{})
	if err != nil {
		errMsg := fmt.Sprintf("failed to create PodSnapshotManualTrigger: %v", err)
		if k8serrors.IsForbidden(err) {
			errMsg += "; check that the service account has RBAC permission to create PodSnapshotManualTrigger resources"
		}
		e.log.Error(err, "failed to create snapshot trigger", "trigger", safeName)
		return SnapshotResponse{
			Success:     false,
			TriggerName: safeName,
			ErrorReason: errMsg,
			ErrorCode:   ErrorCode,
		}
	}

	e.mu.Lock()
	e.createdManualTriggers = append(e.createdManualTriggers, safeName)
	e.mu.Unlock()

	resourceVersion := created.GetResourceVersion()

	result, err := waitForSnapshotCompleted(ctx, e.dynClient, e.namespace, safeName, resourceVersion, timeout, e.log)
	if err != nil {
		e.log.Error(err, "snapshot creation failed", "trigger", safeName)
		return SnapshotResponse{
			Success:     false,
			TriggerName: safeName,
			ErrorReason: err.Error(),
			ErrorCode:   ErrorCode,
		}
	}

	return SnapshotResponse{
		Success:           true,
		TriggerName:       safeName,
		SnapshotUID:       result.SnapshotUID,
		SnapshotTimestamp: result.SnapshotTimestamp,
		ErrorCode:         SuccessCode,
	}
}

// List returns snapshots matching the filter, sorted newest-first.
func (e *SnapshotEngine) List(ctx context.Context, filter SnapshotFilter) ListSnapshotResult {
	podName, err := e.getPodName(ctx)
	if err != nil || podName == "" {
		return ListSnapshotResult{
			Success:     false,
			ErrorReason: "pod name not available",
			ErrorCode:   ErrorCode,
		}
	}

	hash, err := e.getSandboxNameHash(ctx)
	if err != nil || hash == "" {
		return ListSnapshotResult{
			Success:     false,
			ErrorReason: fmt.Sprintf("sandbox name hash not available: %v", err),
			ErrorCode:   ErrorCode,
		}
	}

	var sb strings.Builder
	sb.WriteString(SandboxNameHashLabel + "=" + hash)
	if len(filter.GroupingLabels) > 0 {
		// Sort keys for a deterministic selector string.
		keys := make([]string, 0, len(filter.GroupingLabels))
		for k := range filter.GroupingLabels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			sb.WriteByte(',')
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(filter.GroupingLabels[k])
		}
	}
	labelSelector := sb.String()

	e.log.Info("listing snapshots", "labelSelector", labelSelector)

	list, err := e.dynClient.Resource(snapshotGVR).Namespace(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		e.log.Error(err, "failed to list snapshots")
		return ListSnapshotResult{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to list PodSnapshots: %v", err),
			ErrorCode:   ErrorCode,
		}
	}

	var details []SnapshotDetail
	for _, item := range list.Items {
		conditions, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
		isReady := false
		for _, raw := range conditions {
			cond, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if cond["type"] == "Ready" && cond["status"] == "True" {
				isReady = true
				break
			}
		}
		if filter.ReadyOnly && !isReady {
			continue
		}

		annotations := item.GetAnnotations()
		sourcePod := "Unknown"
		if annotations != nil {
			if p, ok := annotations[PodSnapshotPodAnnotation]; ok {
				sourcePod = p
			}
		}

		ts := item.GetCreationTimestamp()
		var creationTS time.Time
		if !ts.IsZero() {
			creationTS = ts.Time
		}

		status := "NotReady"
		if isReady {
			status = "Ready"
		}

		details = append(details, SnapshotDetail{
			SnapshotUID:       item.GetName(),
			SourcePod:         sourcePod,
			CreationTimestamp: creationTS,
			Status:            status,
		})
	}

	slices.SortFunc(details, func(a, b SnapshotDetail) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp)
	})

	e.log.Info("found snapshots", "count", len(details))
	return ListSnapshotResult{
		Success:   true,
		Snapshots: details,
		ErrorCode: SuccessCode,
	}
}

// Delete deletes a single snapshot by UID and waits for confirmation.
func (e *SnapshotEngine) Delete(ctx context.Context, snapshotUID string, timeout time.Duration) DeleteSnapshotResult {
	return e.executeDeletion(ctx, snapshotUID, "", nil, timeout)
}

// DeleteAll deletes snapshots matching the given strategy.
// deleteBy must be "all" (delete every snapshot for this sandbox) or "labels"
// (delete snapshots matching labelValue).
func (e *SnapshotEngine) DeleteAll(ctx context.Context, deleteBy string, labelValue map[string]string, timeout time.Duration) (DeleteSnapshotResult, error) {
	switch deleteBy {
	case "all":
		e.log.Info("deleting all snapshots for this sandbox")
		return e.executeDeletion(ctx, "", "global", nil, timeout), nil
	case "labels":
		if len(labelValue) == 0 {
			return DeleteSnapshotResult{}, fmt.Errorf("labelValue must be non-empty when deleteBy=labels")
		}
		e.log.Info("deleting snapshots matching labels", "labels", labelValue)
		return e.executeDeletion(ctx, "", "", labelValue, timeout), nil
	default:
		return DeleteSnapshotResult{}, fmt.Errorf("unsupported deleteBy value %q; must be \"all\" or \"labels\"", deleteBy)
	}
}

// DeleteManualTriggers cleans up PodSnapshotManualTrigger resources created by
// this engine. Best-effort with up to maxRetries attempts; respects ctx cancellation.
func (e *SnapshotEngine) DeleteManualTriggers(ctx context.Context) {
	const maxRetries = 3
	const retryDelay = time.Second

	e.mu.Lock()
	remaining := make([]string, len(e.createdManualTriggers))
	copy(remaining, e.createdManualTriggers)
	e.mu.Unlock()

	for attempt := 1; attempt <= maxRetries && len(remaining) > 0; attempt++ {
		var failed []string
		for _, name := range remaining {
			err := e.dynClient.Resource(triggerGVR).Namespace(e.namespace).Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					e.log.V(1).Info("trigger already deleted", "trigger", name)
					continue
				}
				e.log.Error(err, "failed to delete trigger", "trigger", name, "attempt", attempt, "maxRetries", maxRetries)
				failed = append(failed, name)
			} else {
				e.log.Info("deleted snapshot trigger", "trigger", name)
			}
		}
		remaining = failed
		if len(remaining) > 0 && attempt < maxRetries {
			select {
			case <-ctx.Done():
				goto done
			case <-time.After(retryDelay):
			}
		}
	}

done:
	e.mu.Lock()
	e.createdManualTriggers = remaining
	e.mu.Unlock()

	if len(remaining) > 0 {
		e.log.Info("leaked PodSnapshotManualTrigger resources require manual cleanup",
			"count", len(remaining), "triggers", remaining)
	}
}

func (e *SnapshotEngine) executeDeletion(
	ctx context.Context,
	snapshotUID string,
	scope string,
	labels map[string]string,
	timeout time.Duration,
) DeleteSnapshotResult {
	var toDelete []string

	switch {
	case snapshotUID != "":
		toDelete = []string{snapshotUID}
	case scope == "global":
		result := e.List(ctx, SnapshotFilter{ReadyOnly: false})
		if !result.Success {
			return DeleteSnapshotResult{
				Success:     false,
				ErrorReason: "failed to list snapshots before deletion: " + result.ErrorReason,
				ErrorCode:   ErrorCode,
			}
		}
		for _, s := range result.Snapshots {
			toDelete = append(toDelete, s.SnapshotUID)
		}
	case len(labels) > 0:
		result := e.List(ctx, SnapshotFilter{ReadyOnly: false, GroupingLabels: labels})
		if !result.Success {
			return DeleteSnapshotResult{
				Success:     false,
				ErrorReason: "failed to list snapshots before deletion: " + result.ErrorReason,
				ErrorCode:   ErrorCode,
			}
		}
		for _, s := range result.Snapshots {
			toDelete = append(toDelete, s.SnapshotUID)
		}
	}

	if len(toDelete) == 0 {
		e.log.Info("no snapshots found to delete")
		return DeleteSnapshotResult{Success: true, ErrorCode: SuccessCode}
	}

	var deleted []string
	var errs []string

	for _, uid := range toDelete {
		err := e.dynClient.Resource(snapshotGVR).Namespace(e.namespace).Delete(ctx, uid, metav1.DeleteOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				e.log.V(1).Info("snapshot already deleted", "uid", uid)
				deleted = append(deleted, uid)
				continue
			}
			msg := fmt.Sprintf("failed to delete snapshot %q: %v", uid, err)
			e.log.Error(err, "failed to delete snapshot", "uid", uid)
			errs = append(errs, msg)
			continue
		}

		if err := waitForSnapshotDeletion(ctx, e.dynClient, e.namespace, uid, timeout, e.log); err != nil {
			msg := fmt.Sprintf("timed out waiting for snapshot %q deletion: %v", uid, err)
			e.log.Info(msg)
			errs = append(errs, msg)
			continue
		}
		deleted = append(deleted, uid)
	}

	if len(errs) > 0 {
		errMsg := strings.Join(errs, "; ")
		if len(deleted) > 0 {
			errMsg = fmt.Sprintf("partial failure: deleted %d/%d snapshots; %s", len(deleted), len(toDelete), errMsg)
		}
		return DeleteSnapshotResult{
			Success:          false,
			DeletedSnapshots: deleted,
			ErrorReason:      errMsg,
			ErrorCode:        ErrorCode,
		}
	}

	return DeleteSnapshotResult{
		Success:          true,
		DeletedSnapshots: deleted,
		ErrorCode:        SuccessCode,
	}
}

// sanitizeTriggerName converts an arbitrary string to a valid Kubernetes
// resource name and appends a timestamp+UUID suffix for uniqueness.
// Output always matches [a-z0-9]([-a-z0-9]*[a-z0-9])? and is ≤ 63 chars.
func sanitizeTriggerName(name string) string {
	safe := strings.ToLower(name)

	// Replace every character that is not [a-z0-9-] with a dash.
	var b strings.Builder
	for _, r := range safe {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	safe = b.String()

	// Collapse consecutive dashes produced by replacements.
	for strings.Contains(safe, "--") {
		safe = strings.ReplaceAll(safe, "--", "-")
	}

	// "-YYYYMMDD-HHMMSS-xxxxxxxx" is 25 chars; leave 38 for the base.
	if len(safe) > 38 {
		safe = safe[:38]
	}
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "snap"
	}
	ts := time.Now().UTC().Format("20060102-150405")
	suffix := uuid.New().String()[:8]
	return fmt.Sprintf("%s-%s-%s", safe, ts, suffix)
}
