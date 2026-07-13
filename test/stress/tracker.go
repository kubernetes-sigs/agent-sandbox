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

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// Phase identifies which part of the stress test a Sandbox belongs to.
type Phase string

const (
	// PhaseFill sandboxes provide background scale; they run until the test ends.
	PhaseFill Phase = "fill"
	// PhaseProbe sandboxes measure launch latency against the filled cluster.
	PhaseProbe Phase = "probe"
	// PhaseThroughput sandboxes are churned (create -> ready -> delete) to measure sustained throughput.
	PhaseThroughput Phase = "throughput"
)

// SandboxRecord tracks the lifecycle milestones of a single Sandbox and its backing Pod.
// Client-observed timestamps are taken with time.Now() when we issue a request or observe
// a watch event; server timestamps come from the objects themselves (typically 1s granularity).
// All fields are guarded by the Tracker mutex.
type SandboxRecord struct {
	Name      string
	Namespace string
	Phase     Phase

	// Pod identity, for joining against node-side data sources.
	// PodUID is the pod's metadata.uid. NodeName selects the right
	// profiler-*-<node>.log. ContainerID is the main container's runtime ID
	// with the scheme (e.g. "containerd://") stripped, matching the
	// container_id in containerd's event stream (ctr events).
	PodUID      string
	NodeName    string
	ContainerID string

	// Client-observed milestones.
	CreateCalled    time.Time // just before the Create API call
	CreateReturned  time.Time // Create API call returned successfully
	PodCreated      time.Time // first watch event for the backing Pod
	PodScheduled    time.Time // Pod observed with condition PodScheduled=True
	PodRunning      time.Time // Pod observed with phase=Running
	PodReady        time.Time // Pod observed with condition Ready=True
	SandboxReady    time.Time // Sandbox observed with condition Ready=True
	SandboxFinished time.Time // Sandbox observed with condition Finished=True
	DeleteCalled    time.Time // just before the Delete API call
	PodDeleted      time.Time // watch DELETED event for the backing Pod
	SandboxDeleted  time.Time // watch DELETED event for the Sandbox

	// Server-reported timestamps for cross-checking watch lag
	// (1s granularity; may be skewed relative to the client clock).
	ServerSandboxCreated time.Time // sandbox metadata.creationTimestamp
	ServerPodCreated     time.Time // pod metadata.creationTimestamp
	ServerPodScheduled   time.Time // PodScheduled condition lastTransitionTime
	ServerPodReady       time.Time // pod Ready condition lastTransitionTime
	ServerSandboxReady   time.Time // sandbox Ready condition lastTransitionTime

	Error string

	readyCh     chan struct{}
	readyClosed bool
	goneCh      chan struct{}
	goneClosed  bool
}

// Tracker correlates watch events with the Sandboxes created by the test,
// building a per-sandbox lifecycle record.
type Tracker struct {
	mu      sync.Mutex
	records map[types.NamespacedName]*SandboxRecord
}

func NewTracker() *Tracker {
	return &Tracker{
		records: make(map[types.NamespacedName]*SandboxRecord),
	}
}

// Register creates a record for a Sandbox we are about to create,
// stamping CreateCalled with the current time.
func (t *Tracker) Register(id types.NamespacedName, phase Phase) *SandboxRecord {
	rec := &SandboxRecord{
		Name:         id.Name,
		Namespace:    id.Namespace,
		Phase:        phase,
		CreateCalled: time.Now(),
		readyCh:      make(chan struct{}),
		goneCh:       make(chan struct{}),
	}
	t.mu.Lock()
	t.records[id] = rec
	t.mu.Unlock()
	return rec
}

func (t *Tracker) mutate(id types.NamespacedName, fn func(rec *SandboxRecord)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[id]
	if !ok {
		return
	}
	fn(rec)
}

// MarkCreateReturned records the completion of the Create API call.
func (t *Tracker) MarkCreateReturned(id types.NamespacedName, err error) {
	now := time.Now()
	t.mutate(id, func(rec *SandboxRecord) {
		if err != nil {
			t.setErrorLocked(rec, fmt.Sprintf("create failed: %v", err))
			return
		}
		rec.CreateReturned = now
	})
}

// MarkDeleteCalled records that we issued a Delete for the Sandbox.
func (t *Tracker) MarkDeleteCalled(id types.NamespacedName) {
	now := time.Now()
	t.mutate(id, func(rec *SandboxRecord) {
		if rec.DeleteCalled.IsZero() {
			rec.DeleteCalled = now
		}
	})
}

// MarkError records a per-sandbox error (first error wins).
func (t *Tracker) MarkError(id types.NamespacedName, msg string) {
	t.mutate(id, func(rec *SandboxRecord) {
		t.setErrorLocked(rec, msg)
	})
}

// MarkGone unblocks WaitGone without a watch event, e.g. when a Delete call
// finds the sandbox already absent.
func (t *Tracker) MarkGone(id types.NamespacedName) {
	t.mutate(id, func(rec *SandboxRecord) {
		t.closeGoneLocked(rec)
	})
}

// setErrorLocked must be called with t.mu held.
func (t *Tracker) setErrorLocked(rec *SandboxRecord, msg string) {
	if rec.Error == "" {
		rec.Error = msg
	}
}

// WaitReady blocks until the Sandbox is observed Ready, the timeout expires, or ctx is done.
func (t *Tracker) WaitReady(ctx context.Context, id types.NamespacedName, timeout time.Duration) error {
	rec := t.get(id)
	if rec == nil {
		return fmt.Errorf("no record for sandbox %v", id)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-rec.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timed out after %v waiting for sandbox %v to become ready", timeout, id)
	}
}

// WaitGone blocks until the backing Pod is observed deleted (freeing cluster capacity),
// the timeout expires, or ctx is done.
func (t *Tracker) WaitGone(ctx context.Context, id types.NamespacedName, timeout time.Duration) error {
	rec := t.get(id)
	if rec == nil {
		return fmt.Errorf("no record for sandbox %v", id)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-rec.goneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timed out after %v waiting for sandbox %v pod to be deleted", timeout, id)
	}
}

func (t *Tracker) get(id types.NamespacedName) *SandboxRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.records[id]
}

// Records returns a copy of all records, safe to read without locking.
func (t *Tracker) Records() []SandboxRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SandboxRecord, 0, len(t.records))
	for _, rec := range t.records {
		c := *rec
		c.readyCh = nil
		c.goneCh = nil
		out = append(out, c)
	}
	return out
}

// PhaseCounts summarizes progress for one phase.
type PhaseCounts struct {
	Registered int
	Created    int
	Ready      int
	Finished   int
	Deleted    int
	Failed     int
}

// Snapshot returns per-phase progress counts.
func (t *Tracker) Snapshot() map[Phase]PhaseCounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[Phase]PhaseCounts)
	for _, rec := range t.records {
		c := out[rec.Phase]
		c.Registered++
		if !rec.CreateReturned.IsZero() {
			c.Created++
		}
		if !rec.SandboxReady.IsZero() {
			c.Ready++
		}
		if !rec.SandboxFinished.IsZero() {
			c.Finished++
		}
		if !rec.PodDeleted.IsZero() || !rec.SandboxDeleted.IsZero() {
			c.Deleted++
		}
		if rec.Error != "" {
			c.Failed++
		}
		out[rec.Phase] = c
	}
	return out
}

// HandleWatchEvent updates milestone records from a watch event.
// It must be fast: it runs on the watch decode path.
func (t *Tracker) HandleWatchEvent(resource string, eventType watch.EventType, u *unstructured.Unstructured) {
	switch resource {
	case "sandboxes":
		t.handleSandboxEvent(eventType, u)
	case "pods":
		t.handlePodEvent(eventType, u)
	}
}

func (t *Tracker) handleSandboxEvent(eventType watch.EventType, u *unstructured.Unstructured) {
	id := types.NamespacedName{Name: u.GetName(), Namespace: u.GetNamespace()}
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[id]
	if !ok {
		return
	}

	if eventType == watch.Deleted {
		if rec.SandboxDeleted.IsZero() {
			rec.SandboxDeleted = now
		}
		// If no Pod was ever observed there is nothing else to wait for.
		if rec.PodCreated.IsZero() {
			t.closeGoneLocked(rec)
		}
		return
	}

	if rec.ServerSandboxCreated.IsZero() {
		rec.ServerSandboxCreated = u.GetCreationTimestamp().Time
	}

	if ready, ltt := conditionTrue(u, "Ready"); ready && rec.SandboxReady.IsZero() {
		rec.SandboxReady = now
		rec.ServerSandboxReady = ltt
		t.closeReadyLocked(rec)
	}

	if finished, _ := conditionTrue(u, "Finished"); finished && rec.SandboxFinished.IsZero() {
		rec.SandboxFinished = now
	}
}

func (t *Tracker) handlePodEvent(eventType watch.EventType, u *unstructured.Unstructured) {
	// The controller names the backing Pod after the Sandbox
	// (replacement pods can differ, but stress sandboxes are never replaced).
	id := types.NamespacedName{Name: u.GetName(), Namespace: u.GetNamespace()}
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[id]
	if !ok {
		return
	}

	if eventType == watch.Deleted {
		if rec.PodDeleted.IsZero() {
			rec.PodDeleted = now
		}
		t.closeGoneLocked(rec)
		return
	}

	if rec.PodCreated.IsZero() {
		rec.PodCreated = now
		rec.ServerPodCreated = u.GetCreationTimestamp().Time
		rec.PodUID = string(u.GetUID())
	}

	if rec.NodeName == "" {
		if nodeName, _, _ := unstructured.NestedString(u.Object, "spec", "nodeName"); nodeName != "" {
			rec.NodeName = nodeName
		}
	}

	if rec.ContainerID == "" {
		rec.ContainerID = mainContainerID(u)
	}

	if scheduled, ltt := conditionTrue(u, "PodScheduled"); scheduled && rec.PodScheduled.IsZero() {
		rec.PodScheduled = now
		rec.ServerPodScheduled = ltt
	}

	if phase, _, _ := unstructured.NestedString(u.Object, "status", "phase"); phase == "Running" && rec.PodRunning.IsZero() {
		// Client-observed: first watch event where phase=Running.
		// Prefer this over containerStatuses[].state.running.startedAt, which
		// is on the node's clock and can be skewed relative to ours.
		rec.PodRunning = now
	}

	if ready, ltt := conditionTrue(u, "Ready"); ready && rec.PodReady.IsZero() {
		rec.PodReady = now
		rec.ServerPodReady = ltt
	}
}

// closeReadyLocked must be called with t.mu held.
func (t *Tracker) closeReadyLocked(rec *SandboxRecord) {
	if !rec.readyClosed {
		rec.readyClosed = true
		close(rec.readyCh)
	}
}

// closeGoneLocked must be called with t.mu held.
func (t *Tracker) closeGoneLocked(rec *SandboxRecord) {
	if !rec.goneClosed {
		rec.goneClosed = true
		close(rec.goneCh)
	}
}

// mainContainerID returns the pod's main container runtime ID with the
// scheme prefix (e.g. "containerd://") stripped, or "" if not yet assigned.
// The runtime only assigns the ID once the container is created on the node.
func mainContainerID(u *unstructured.Unstructured) string {
	statuses, found, err := unstructured.NestedSlice(u.Object, "status", "containerStatuses")
	if err != nil || !found {
		return ""
	}
	for _, sVal := range statuses {
		s, ok := sVal.(map[string]any)
		if !ok {
			continue
		}
		containerID, _ := s["containerID"].(string)
		if containerID == "" {
			continue
		}
		if _, id, ok := strings.Cut(containerID, "://"); ok {
			return id
		}
		return containerID
	}
	return ""
}

// conditionTrue reports whether the given condition type has status True,
// and returns its lastTransitionTime if present.
func conditionTrue(u *unstructured.Unstructured, condType string) (bool, time.Time) {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false, time.Time{}
	}
	for _, condVal := range conditions {
		cond, ok := condVal.(map[string]any)
		if !ok {
			continue
		}
		cType, _ := cond["type"].(string)
		cStatus, _ := cond["status"].(string)
		if cType == condType && cStatus == "True" {
			var ltt time.Time
			if s, ok := cond["lastTransitionTime"].(string); ok {
				if parsed, err := time.Parse(time.RFC3339, s); err == nil {
					ltt = parsed
				}
			}
			return true, ltt
		}
	}
	return false, time.Time{}
}
