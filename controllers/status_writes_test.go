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

package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func condition(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason}
}

func statusWithConditions(conds ...metav1.Condition) *sandboxv1beta1.SandboxStatus {
	return &sandboxv1beta1.SandboxStatus{Conditions: conds}
}

func TestMaterialStatusChange(t *testing.T) {
	ready := string(sandboxv1beta1.SandboxConditionReady)
	suspended := string(sandboxv1beta1.SandboxConditionSuspended)
	finished := string(sandboxv1beta1.SandboxConditionFinished)

	tests := []struct {
		name     string
		old, new *sandboxv1beta1.SandboxStatus
		material bool
	}{
		{
			name: "initial fill: empty -> Ready=False + Suspended=False is transitional",
			old:  &sandboxv1beta1.SandboxStatus{},
			new: statusWithConditions(
				condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady),
				condition(suspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonNotSuspended),
			),
			material: false,
		},
		{
			name:     "Ready False -> True is material",
			old:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			new:      statusWithConditions(condition(ready, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonDependenciesReady)),
			material: true,
		},
		{
			name:     "Ready True -> False (regression) is material",
			old:      statusWithConditions(condition(ready, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonDependenciesReady)),
			new:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			material: true,
		},
		{
			name:     "reason/message churn while Ready stays False is transitional",
			old:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			new:      statusWithConditions(condition(ready, metav1.ConditionFalse, "ReconcilerError")),
			material: false,
		},
		{
			name:     "Ready reason moving to Expired is material even without a value flip",
			old:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			new:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonExpired)),
			material: true,
		},
		{
			name:     "Expired appearing on a status with no prior Ready condition is material",
			old:      &sandboxv1beta1.SandboxStatus{},
			new:      statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonExpired)),
			material: true,
		},
		{
			name: "Finished appearing (Status=True) is material",
			old:  statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			new: statusWithConditions(
				condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonPodSucceeded),
				condition(finished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded),
			),
			material: true,
		},
		{
			name:     "condition removal is material",
			old:      statusWithConditions(condition(finished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded)),
			new:      &sandboxv1beta1.SandboxStatus{},
			material: true,
		},
		{
			name:     "Suspended False -> Unknown is material",
			old:      statusWithConditions(condition(suspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonNotSuspended)),
			new:      statusWithConditions(condition(suspended, metav1.ConditionUnknown, sandboxv1beta1.SandboxReasonSuspendedPodStateUnknown)),
			material: true,
		},
		{
			name: "field-only fill (nodeName, podIPs, selector) is transitional",
			old:  statusWithConditions(condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)),
			new: &sandboxv1beta1.SandboxStatus{
				Conditions:    []metav1.Condition{condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady)},
				NodeName:      "node-1",
				PodIPs:        []string{"10.0.0.1"},
				LabelSelector: "x=y",
			},
			material: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := materialStatusChange(tt.old, tt.new); got != tt.material {
				t.Errorf("materialStatusChange() = %v, want %v", got, tt.material)
			}
		})
	}
}

// TestUpdateStatusTransitionalWindow exercises the creation-age gating end
// to end against the fake client: transitional changes on a young sandbox
// are deferred (no write, positive requeue), material changes and old
// sandboxes write through.
func TestUpdateStatusTransitionalWindow(t *testing.T) {
	ready := string(sandboxv1beta1.SandboxConditionReady)
	const window = 15 * time.Second

	newSandbox := func(age time.Duration) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "sb",
				Namespace:         "default",
				UID:               sandboxUID,
				CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			},
		}
	}

	tests := []struct {
		name      string
		age       time.Duration
		mutate    func(*sandboxv1beta1.Sandbox)
		wantWrite bool
		wantDefer bool
	}{
		{
			name: "transitional change on young sandbox is deferred",
			age:  1 * time.Second,
			mutate: func(sb *sandboxv1beta1.Sandbox) {
				sb.Status.Conditions = []metav1.Condition{
					condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady),
				}
				sb.Status.NodeName = "node-1"
			},
			wantWrite: false,
			wantDefer: true,
		},
		{
			name: "material change on young sandbox writes immediately",
			age:  1 * time.Second,
			mutate: func(sb *sandboxv1beta1.Sandbox) {
				sb.Status.Conditions = []metav1.Condition{
					condition(ready, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonDependenciesReady),
				}
			},
			wantWrite: true,
			wantDefer: false,
		},
		{
			name: "transitional change past the window writes through",
			age:  window + time.Second,
			mutate: func(sb *sandboxv1beta1.Sandbox) {
				sb.Status.Conditions = []metav1.Condition{
					condition(ready, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady),
				}
				sb.Status.PodIPs = []string{"10.0.0.1"}
			},
			wantWrite: true,
			wantDefer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newSandbox(tt.age)
			fakeClient := newFakeClient(sb)
			r := &SandboxReconciler{Client: fakeClient, TransitionalStatusWindow: window}

			oldStatus := sb.Status.DeepCopy()
			tt.mutate(sb)

			wait, err := r.updateStatus(context.Background(), oldStatus, sb)
			if err != nil {
				t.Fatalf("updateStatus() error: %v", err)
			}
			if tt.wantDefer && (wait <= 0 || wait > window) {
				t.Errorf("updateStatus() wait = %v, want in (0, %v]", wait, window)
			}
			if !tt.wantDefer && wait != 0 {
				t.Errorf("updateStatus() wait = %v, want 0", wait)
			}

			stored := &sandboxv1beta1.Sandbox{}
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sb"}, stored); err != nil {
				t.Fatalf("Get: %v", err)
			}
			wrote := len(stored.Status.Conditions) > 0 || stored.Status.NodeName != "" || len(stored.Status.PodIPs) > 0
			if wrote != tt.wantWrite {
				t.Errorf("status written = %v, want %v (stored status: %+v)", wrote, tt.wantWrite, stored.Status)
			}
		})
	}
}

// TestUpdateStatusWindowDisabled verifies the default (window 0) keeps the
// fully synchronous behavior for transitional changes.
func TestUpdateStatusWindowDisabled(t *testing.T) {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sb", Namespace: "default", UID: sandboxUID,
			CreationTimestamp: metav1.Now(),
		},
	}
	fakeClient := newFakeClient(sb)
	r := &SandboxReconciler{Client: fakeClient}

	oldStatus := sb.Status.DeepCopy()
	sb.Status.Conditions = []metav1.Condition{
		condition(string(sandboxv1beta1.SandboxConditionReady), metav1.ConditionFalse, sandboxv1beta1.SandboxReasonDependenciesNotReady),
	}
	// Include a second field so the change is not nodeName-only.
	sb.Status.LabelSelector = "x=y"

	wait, err := r.updateStatus(context.Background(), oldStatus, sb)
	if err != nil {
		t.Fatalf("updateStatus() error: %v", err)
	}
	if wait != 0 {
		t.Errorf("wait = %v, want 0 with window disabled", wait)
	}
	stored := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sb"}, stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Status.Conditions) == 0 {
		t.Error("status not written with window disabled")
	}
}

func TestStaleCacheGuard(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "sb"}
	other := types.NamespacedName{Namespace: "default", Name: "other"}

	var g staleCacheGuard

	// No record: never stale.
	if g.stillStale(key, "5") {
		t.Error("stillStale with no record = true, want false")
	}

	// Recorded write 5 -> 6: observing 5 is stale (event pending), and stays
	// stale on repeat observation.
	g.record(key, "5", "6")
	if !g.stillStale(key, "5") {
		t.Error("observing pre-write rv: stillStale = false, want true")
	}
	if !g.stillStale(key, "5") {
		t.Error("repeat observation of pre-write rv: stillStale = false, want true")
	}
	// Observing the post-write rv clears the record.
	if g.stillStale(key, "6") {
		t.Error("observing post-write rv: stillStale = true, want false")
	}
	if g.stillStale(key, "5") {
		t.Error("record should have been dropped after catch-up")
	}

	// A third-party write (any rv other than pre-write) also clears.
	g.record(key, "6", "7")
	if g.stillStale(key, "9") {
		t.Error("observing newer third-party rv: stillStale = true, want false")
	}

	// No-op writes must not be recorded: they produce no event to clear them.
	g.record(key, "7", "7")
	if g.stillStale(key, "7") {
		t.Error("no-op write recorded: every future pass would be stale forever")
	}
	g.record(key, "7", "")
	if g.stillStale(key, "7") {
		t.Error("empty rvAfter recorded")
	}

	// clear is per-key.
	g.record(key, "7", "8")
	g.record(other, "1", "2")
	g.clear(key)
	if g.stillStale(key, "7") {
		t.Error("cleared key still stale")
	}
	if !g.stillStale(other, "1") {
		t.Error("clear(key) affected other key")
	}
}
