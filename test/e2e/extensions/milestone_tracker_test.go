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

package extensions

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

var claimGVR = schema.GroupVersionResource{
	Group:    "extensions.agents.x-k8s.io",
	Version:  "v1beta1",
	Resource: "sandboxclaims",
}

type claimMilestones struct {
	claimName   string
	sandboxName string

	createCalled   time.Time
	createReturned time.Time
	adopted        time.Time // watch: status.sandbox.name first set
	claimReady     time.Time // watch: Ready=True

	serverClaimCreated time.Time
	serverPodCreated   time.Time
	serverPodScheduled time.Time
	serverPodReady     time.Time
	serverSandboxReady time.Time

	ready chan struct{}
}

type milestoneBreakdown struct {
	CreateAckMs float64
	AdoptionMs  float64
	ScheduleMs  float64
	RuntimeMs   float64
	PropagateMs float64
	EndToEndMs  float64
	IsWarm      bool
}

type milestoneTracker struct {
	mu        sync.Mutex
	records   map[string]*claimMilestones
	dynClient dynamic.Interface
	ns        string
	cancel    context.CancelFunc
	started   chan struct{}
}

func newMilestoneTracker(ctx context.Context, dynClient dynamic.Interface, ns string) *milestoneTracker {
	watchCtx, cancel := context.WithCancel(ctx)
	t := &milestoneTracker{
		records:   make(map[string]*claimMilestones),
		dynClient: dynClient,
		ns:        ns,
		cancel:    cancel,
		started:   make(chan struct{}),
	}
	go t.watchClaims(watchCtx)
	<-t.started
	return t
}

func (t *milestoneTracker) Stop() {
	t.cancel()
}

func (t *milestoneTracker) Register(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records[name] = &claimMilestones{
		claimName: name,
		ready:     make(chan struct{}),
	}
}

func (t *milestoneTracker) MarkCreateCalled(name string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.records[name]; ok {
		rec.createCalled = ts
	}
}

func (t *milestoneTracker) MarkCreateReturned(name string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.records[name]; ok {
		rec.createReturned = ts
	}
}

func (t *milestoneTracker) WaitReady(ctx context.Context, name string) error {
	t.mu.Lock()
	rec, ok := t.records[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("claim %q not registered", name)
	}
	select {
	case <-rec.ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for claim %q to become ready: %w", name, ctx.Err())
	}
}

func (t *milestoneTracker) CollectBreakdown(ctx context.Context, cl *framework.ClusterClient, name string) (milestoneBreakdown, error) {
	t.mu.Lock()
	rec, ok := t.records[name]
	t.mu.Unlock()
	if !ok {
		return milestoneBreakdown{}, fmt.Errorf("claim %q not registered", name)
	}
	if rec.sandboxName == "" {
		return milestoneBreakdown{}, fmt.Errorf("claim %q has no bound sandbox", name)
	}

	sandboxID := types.NamespacedName{Name: rec.sandboxName, Namespace: t.ns}
	sandbox, err := cl.GetSandbox(ctx, sandboxID)
	if err != nil {
		return milestoneBreakdown{}, fmt.Errorf("get sandbox %s: %w", sandboxID, err)
	}
	rec.serverSandboxReady = conditionTransitionTime(sandbox, "Ready")

	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	podID := types.NamespacedName{Name: rec.sandboxName, Namespace: t.ns}
	if err := cl.Get(ctx, podID, pod); err != nil {
		return milestoneBreakdown{}, fmt.Errorf("get pod %s: %w", podID, err)
	}
	rec.serverPodCreated = pod.GetCreationTimestamp().Time
	rec.serverPodScheduled = conditionTransitionTime(pod, "PodScheduled")
	rec.serverPodReady = conditionTransitionTime(pod, "Ready")

	return computeBreakdown(rec), nil
}

func (t *milestoneTracker) GetMilestones(name string) (claimMilestones, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[name]
	if !ok {
		return claimMilestones{}, false
	}
	return *rec, true
}

func (t *milestoneTracker) watchClaims(ctx context.Context) {
	watcher, err := t.dynClient.Resource(claimGVR).Namespace(t.ns).Watch(ctx, metav1.ListOptions{})
	close(t.started)
	if err != nil {
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			t.handleEvent(event)
		}
	}
}

func (t *milestoneTracker) handleEvent(event watch.Event) {
	u, ok := event.Object.(*unstructured.Unstructured)
	if !ok {
		return
	}
	name := u.GetName()
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	rec, ok := t.records[name]
	if !ok {
		return
	}

	if event.Type == watch.Deleted {
		select {
		case <-rec.ready:
		default:
			close(rec.ready)
		}
		return
	}

	if rec.serverClaimCreated.IsZero() {
		rec.serverClaimCreated = u.GetCreationTimestamp().Time
	}

	if rec.sandboxName == "" {
		if sbName, _, _ := unstructured.NestedString(u.Object, "status", "sandbox", "name"); sbName != "" {
			rec.sandboxName = sbName
			if rec.adopted.IsZero() {
				rec.adopted = now
			}
		}
	}

	if rec.claimReady.IsZero() {
		if isConditionTrue(u, "Ready") {
			rec.claimReady = now
			select {
			case <-rec.ready:
			default:
				close(rec.ready)
			}
		}
	}
}

func computeBreakdown(rec *claimMilestones) milestoneBreakdown {
	b := milestoneBreakdown{
		EndToEndMs: msInterval(rec.createCalled, rec.claimReady),
	}
	if !rec.createCalled.IsZero() && !rec.createReturned.IsZero() {
		b.CreateAckMs = msInterval(rec.createCalled, rec.createReturned)
	}
	if !rec.createReturned.IsZero() && !rec.claimReady.IsZero() {
		b.AdoptionMs = msInterval(rec.createReturned, rec.claimReady)
	}

	b.IsWarm = !rec.serverPodCreated.IsZero() && !rec.createCalled.IsZero() &&
		rec.serverPodCreated.Before(rec.createCalled)

	if !b.IsWarm {
		if !rec.serverPodCreated.IsZero() && !rec.serverPodScheduled.IsZero() {
			b.ScheduleMs = msInterval(rec.serverPodCreated, rec.serverPodScheduled)
		}
		if !rec.serverPodScheduled.IsZero() && !rec.serverPodReady.IsZero() {
			b.RuntimeMs = msInterval(rec.serverPodScheduled, rec.serverPodReady)
		}
	}

	if !rec.serverSandboxReady.IsZero() && !rec.claimReady.IsZero() {
		b.PropagateMs = msInterval(rec.serverSandboxReady, rec.claimReady)
	}

	return b
}

func msInterval(from, to time.Time) float64 {
	return float64(to.Sub(from)) / float64(time.Millisecond)
}

func isConditionTrue(u *unstructured.Unstructured, condType string) bool {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, condVal := range conditions {
		cond, ok := condVal.(map[string]any)
		if !ok {
			continue
		}
		cType, _ := cond["type"].(string)
		cStatus, _ := cond["status"].(string)
		if cType == condType && cStatus == "True" {
			return true
		}
	}
	return false
}

func conditionTransitionTime(u *unstructured.Unstructured, condType string) time.Time {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return time.Time{}
	}
	for _, condVal := range conditions {
		cond, ok := condVal.(map[string]any)
		if !ok {
			continue
		}
		cType, _ := cond["type"].(string)
		cStatus, _ := cond["status"].(string)
		if cType == condType && cStatus == "True" {
			if s, ok := cond["lastTransitionTime"].(string); ok {
				if parsed, err := time.Parse(time.RFC3339, s); err == nil {
					return parsed
				}
			}
		}
	}
	return time.Time{}
}
