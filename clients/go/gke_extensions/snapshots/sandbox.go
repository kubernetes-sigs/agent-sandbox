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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	sandbox "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// SandboxWithSnapshotSupport wraps a sandbox.Handle and sandbox.Info with
// GKE Pod Snapshot lifecycle operations (suspend, resume, snapshot CRUD).
type SandboxWithSnapshotSupport struct {
	sandbox.Handle
	sandbox.Info

	k8s       *sandbox.K8sHelper
	namespace string
	log       logr.Logger

	mu           sync.Mutex
	snapshotHash string // cached sandbox-name-hash value
	engine       *SnapshotEngine
}

// NewSandboxWithSnapshotSupport wraps an existing sandbox handle with snapshot support.
// handle and info are typically the same *sandbox.Sandbox.
func NewSandboxWithSnapshotSupport(
	handle sandbox.Handle,
	info sandbox.Info,
	k8s *sandbox.K8sHelper,
	namespace string,
	log logr.Logger,
) *SandboxWithSnapshotSupport {
	return &SandboxWithSnapshotSupport{
		Handle:    handle,
		Info:      info,
		k8s:       k8s,
		namespace: namespace,
		log:       log,
	}
}

// IsActive reports whether the sandbox handle is ready for communication and
// the snapshot engine has been initialised.
// Mirrors Python's SandboxWithSnapshotSupport.is_active().
func (s *SandboxWithSnapshotSupport) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Handle.IsReady() && s.engine != nil
}

// Snapshots returns the SnapshotEngine for this sandbox, initialising it lazily.
func (s *SandboxWithSnapshotSupport) Snapshots() *SnapshotEngine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		s.engine = NewSnapshotEngine(
			s.namespace,
			s.k8s,
			func(_ context.Context) (string, error) {
				name := s.Info.PodName()
				if name == "" {
					return "", fmt.Errorf("pod name not yet available; ensure the sandbox is open")
				}
				return name, nil
			},
			func(ctx context.Context) (string, error) {
				return s.resolveSandboxNameHash(ctx)
			},
			s.log,
		)
	}
	return s.engine
}

// IsSuspended reports whether the sandbox is currently suspended (spec.replicas == 0).
func (s *SandboxWithSnapshotSupport) IsSuspended(ctx context.Context) (bool, error) {
	sb, err := s.k8s.AgentsClient.Sandboxes(s.namespace).Get(ctx, s.Info.SandboxName(), metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("checking suspend state: %w", err)
	}
	if sb.Spec.Replicas != nil && *sb.Spec.Replicas == 0 {
		return true, nil
	}
	return false, nil
}

// IsRestoredFromSnapshot checks whether the sandbox pod was restored from snapshotUID.
func (s *SandboxWithSnapshotSupport) IsRestoredFromSnapshot(ctx context.Context, snapshotUID string) RestoreCheckResult {
	if snapshotUID == "" {
		return RestoreCheckResult{
			Success:     false,
			ErrorReason: "snapshotUID cannot be empty",
			ErrorCode:   ErrorCode,
		}
	}
	podName := s.Info.PodName()
	if podName == "" {
		return RestoreCheckResult{
			Success:     false,
			ErrorReason: "pod name not found; ensure sandbox is open",
			ErrorCode:   ErrorCode,
		}
	}
	return checkPodRestoredFromSnapshot(ctx, s.k8s.CoreClient, s.namespace, podName, snapshotUID, s.log)
}

// Suspend suspends the sandbox by scaling it to zero replicas.
// If snapshotBeforeSuspend is true, a snapshot is created first.
// timeout controls how long to wait for the pod to terminate.
func (s *SandboxWithSnapshotSupport) Suspend(ctx context.Context, snapshotBeforeSuspend bool, timeout time.Duration) SuspendResponse {
	suspended, err := s.IsSuspended(ctx)
	if err != nil {
		s.log.Error(err, "failed to check suspend state before suspend")
		return SuspendResponse{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to check suspend state: %v", err),
			ErrorCode:   ErrorCode,
		}
	}
	if suspended {
		s.log.Info("sandbox is already suspended", "sandbox", s.Info.SandboxName())
		return SuspendResponse{Success: true, ErrorCode: SuccessCode}
	}

	// Pre-resolve sandbox name hash while the sandbox is still running.
	// The hash must be cached before scaling to 0 replicas, since the
	// Sandbox CR's status.selector may be cleared once the pod terminates.
	if _, err := s.resolveSandboxNameHash(ctx); err != nil {
		s.log.Error(err, "cannot suspend: failed to resolve sandbox name hash")
		return SuspendResponse{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to resolve sandbox name hash: %v", err),
			ErrorCode:   ErrorCode,
		}
	}

	var snapshotResp *SnapshotResponse
	if snapshotBeforeSuspend {
		triggerName := "suspend-" + s.Info.SandboxName()
		resp := s.Snapshots().Create(ctx, triggerName, timeout)
		if !resp.Success {
			s.log.Error(nil, "snapshot before suspend failed", "reason", resp.ErrorReason)
			return SuspendResponse{
				Success:          false,
				SnapshotResponse: &resp,
				ErrorReason:      "snapshot failed: " + resp.ErrorReason,
				ErrorCode:        ErrorCode,
			}
		}
		snapshotResp = &resp
	}

	// Capture pod UID before scaling down so we can detect termination.
	podName := s.Info.PodName()
	podUID := ""
	if podName != "" {
		pod, err := s.k8s.CoreClient.Pods(s.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			podUID = string(pod.UID)
		}
	}

	if err := s.setReplicas(ctx, 0); err != nil {
		s.log.Error(err, "failed to scale sandbox to 0")
		return SuspendResponse{
			Success:          false,
			SnapshotResponse: snapshotResp,
			ErrorReason:      fmt.Sprintf("failed to scale down sandbox: %v", err),
			ErrorCode:        ErrorCode,
		}
	}
	s.log.Info("sandbox scaled to 0 replicas", "sandbox", s.Info.SandboxName())

	if podName == "" {
		s.log.Info("pod name was unknown at suspend time; skipping termination wait", "sandbox", s.Info.SandboxName())
		return SuspendResponse{Success: true, SnapshotResponse: snapshotResp, ErrorCode: SuccessCode}
	}

	if waitForPodTermination(ctx, s.k8s.CoreClient, s.namespace, podName, podUID, timeout, s.log) {
		s.log.Info("sandbox pod terminated", "sandbox", s.Info.SandboxName())
		return SuspendResponse{
			Success:          true,
			SnapshotResponse: snapshotResp,
			ErrorCode:        SuccessCode,
		}
	}

	s.log.Info("timed out waiting for pod termination", "sandbox", s.Info.SandboxName())
	return SuspendResponse{
		Success:          false,
		SnapshotResponse: snapshotResp,
		ErrorReason:      "timed out waiting for pod to terminate",
		ErrorCode:        ErrorCode,
	}
}

// Resume resumes the sandbox by scaling it to one replica.
// timeout controls how long to wait for the pod to become ready.
func (s *SandboxWithSnapshotSupport) Resume(ctx context.Context, timeout time.Duration) ResumeResponse {
	suspended, err := s.IsSuspended(ctx)
	if err != nil {
		s.log.Error(err, "failed to check suspend state before resume")
		return ResumeResponse{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to check suspend state: %v", err),
			ErrorCode:   ErrorCode,
		}
	}
	if !suspended {
		s.log.Info("sandbox is already running (not suspended)", "sandbox", s.Info.SandboxName())
		return ResumeResponse{Success: true, ErrorCode: SuccessCode}
	}

	// Capture the latest snapshot UID before scaling up to compare after resume.
	listResult := s.Snapshots().List(ctx, SnapshotFilter{ReadyOnly: true})
	if !listResult.Success {
		s.log.Error(nil, "failed to list snapshots before resume", "reason", listResult.ErrorReason)
		return ResumeResponse{
			Success:     false,
			ErrorReason: "failed to list snapshots: " + listResult.ErrorReason,
			ErrorCode:   ErrorCode,
		}
	}
	var latestSnapshotUID string
	if len(listResult.Snapshots) > 0 {
		latestSnapshotUID = listResult.Snapshots[0].SnapshotUID
	}

	if err := s.setReplicas(ctx, 1); err != nil {
		s.log.Error(err, "failed to scale sandbox to 1")
		return ResumeResponse{
			Success:     false,
			ErrorReason: fmt.Sprintf("failed to scale up sandbox: %v", err),
			ErrorCode:   ErrorCode,
		}
	}
	s.log.Info("sandbox scaled to 1 replica", "sandbox", s.Info.SandboxName())

	if !waitForPodReady(ctx, s.k8s.CoreClient, s.namespace, s.Info.PodName, timeout, s.log) {
		s.log.Info("timed out waiting for pod to become ready", "sandbox", s.Info.SandboxName())
		return ResumeResponse{
			Success:     false,
			SnapshotUID: latestSnapshotUID,
			ErrorReason: "timed out waiting for pod to become ready",
			ErrorCode:   ErrorCode,
		}
	}

	if latestSnapshotUID == "" {
		s.log.Info("no previous snapshots found; pod started fresh", "sandbox", s.Info.SandboxName())
		return ResumeResponse{
			Success:              true,
			RestoredFromSnapshot: false,
			ErrorCode:            SuccessCode,
		}
	}

	restoreCheck := s.IsRestoredFromSnapshot(ctx, latestSnapshotUID)
	if restoreCheck.Success {
		s.log.Info("sandbox restored from snapshot", "sandbox", s.Info.SandboxName(), "snapshotUID", latestSnapshotUID)
		return ResumeResponse{
			Success:              true,
			RestoredFromSnapshot: true,
			SnapshotUID:          latestSnapshotUID,
			ErrorCode:            SuccessCode,
		}
	}

	s.log.Error(nil, "pod ready but not restored from snapshot", "reason", restoreCheck.ErrorReason)
	return ResumeResponse{
		Success:              false,
		RestoredFromSnapshot: false,
		SnapshotUID:          latestSnapshotUID,
		ErrorReason:          "pod ready but not restored from snapshot: " + restoreCheck.ErrorReason,
		ErrorCode:            ErrorCode,
	}
}

// Close cleans up snapshot trigger resources and then closes the underlying sandbox.
func (s *SandboxWithSnapshotSupport) Close(ctx context.Context) error {
	s.mu.Lock()
	eng := s.engine
	s.mu.Unlock()

	if eng != nil {
		eng.DeleteManualTriggers(ctx)
	}
	return s.Handle.Close(ctx)
}

// resolveSandboxNameHash fetches (and caches) the sandbox-name-hash label value
// from the Sandbox CR's status.selector field. The provided ctx is forwarded to
// the Kubernetes API call so that caller timeouts and cancellation are respected.
func (s *SandboxWithSnapshotSupport) resolveSandboxNameHash(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.snapshotHash != "" {
		h := s.snapshotHash
		s.mu.Unlock()
		return h, nil
	}
	s.mu.Unlock()

	sb, err := s.k8s.AgentsClient.Sandboxes(s.namespace).Get(ctx, s.Info.SandboxName(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting sandbox CR: %w", err)
	}

	// LabelSelector format: "agents.x-k8s.io/sandbox-name-hash=<value>"
	sel := sb.Status.LabelSelector
	if sel == "" {
		return "", fmt.Errorf("sandbox %q has no status.selector yet; ensure the sandbox is running", s.Info.SandboxName())
	}
	if key, val, found := strings.Cut(sel, "="); found && key == SandboxNameHashLabel && val != "" {
		s.mu.Lock()
		s.snapshotHash = val
		s.mu.Unlock()
		return val, nil
	}
	return "", fmt.Errorf("sandbox %q status.selector %q does not contain expected label %q",
		s.Info.SandboxName(), sel, SandboxNameHashLabel)
}

// setReplicas patches spec.replicas on the Sandbox CR.
func (s *SandboxWithSnapshotSupport) setReplicas(ctx context.Context, replicas int32) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	_, err := s.k8s.AgentsClient.Sandboxes(s.namespace).Patch(
		ctx,
		s.Info.SandboxName(),
		k8stypes.MergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	return err
}
