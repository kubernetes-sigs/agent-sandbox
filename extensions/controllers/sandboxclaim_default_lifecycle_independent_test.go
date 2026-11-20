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
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These are fake-client unit tests of policy resolution, NOT API-server,
// scheduling, garbage-collection, watch-delivery, or resource-reclamation tests.
func independentDefaultFixture() (*ext.SandboxClaim, *ext.SandboxWarmPool) {
	return &ext.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid", CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		Spec:       ext.SandboxClaimSpec{WarmPoolRef: ext.SandboxWarmPoolRef{Name: "pool"}},
	}, &ext.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"}}
}
func independentTTL(n int32) *ext.SandboxClaimDefaults {
	return &ext.SandboxClaimDefaults{TTLSecondsAfterCreated: &n}
}
func independentClient(t *testing.T, claim *ext.SandboxClaim, objects ...client.Object) client.Client {
	t.Helper()
	objects = append(objects, claim)
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objects...).WithStatusSubresource(claim).Build()
}
func independentReadClaim(t *testing.T, c client.Client) *ext.SandboxClaim {
	t.Helper()
	got := &ext.SandboxClaim{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "claim", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIndependentDefaultLifecycleCreationDeadlineFrozen(t *testing.T) {
	ctx := context.Background()
	claim, pool := independentDefaultFixture()
	pool.Spec.ClaimDefaults = independentTTL(120)
	originalSpec := claim.Spec.DeepCopy()
	c := independentClient(t, claim, pool)
	r := &SandboxClaimReconciler{Client: c, APIReader: c}
	stopPass, err := r.initializeDefaultLifecycle(ctx, claim)
	if err != nil || stopPass {
		t.Fatalf("initialize: stopPass=%v err=%v", stopPass, err)
	}
	persisted := independentReadClaim(t, c)
	want := claim.CreationTimestamp.Add(120 * time.Second)
	if persisted.Status.DefaultLifecycle == nil || persisted.Status.DefaultLifecycle.ShutdownTime == nil || !persisted.Status.DefaultLifecycle.ShutdownTime.Time.Equal(want) {
		t.Fatalf("deadline is not creation+TTL: %#v", persisted.Status.DefaultLifecycle)
	}
	if persisted.Status.DefaultLifecycle.ShutdownPolicy != ext.ShutdownPolicyRetain {
		t.Fatal("default must retain claim evidence")
	}
	if !reflect.DeepEqual(originalSpec, &persisted.Spec) {
		t.Fatal("initialization mutated claim spec")
	}
	// The fixed creation timestamp is already in the past. Initialization must
	// not grant a fresh TTL measured from reconcile time.
	expired, left := r.checkExpiration(persisted)
	if !expired || left > 0 {
		t.Fatalf("old claim got a sliding deadline: expired=%v left=%v", expired, left)
	}
	pool.Spec.ClaimDefaults = independentTTL(3600)
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Re-run on a newly fetched object (as after controller restart), not merely
	// the mutated in-memory pointer. Pool edits must not reset the persisted TTL.
	stopPass, err = r.initializeDefaultLifecycle(ctx, persisted)
	if err != nil || stopPass {
		t.Fatalf("second initialization: %v %v", stopPass, err)
	}
	after := independentReadClaim(t, c)
	if !after.Status.DefaultLifecycle.ShutdownTime.Time.Equal(want) {
		t.Fatal("pool edit slid deadline")
	}
	// Expiration consumes only claim state, without fetching a pool or depending
	// on a pool-edit reconcile event.
	if err := c.Delete(ctx, pool); err != nil {
		t.Fatal(err)
	}
	expired, _ = (&SandboxClaimReconciler{}).checkExpiration(after)
	if !expired {
		t.Fatal("persisted deadline depended on pool availability")
	}
}

func TestIndependentDefaultLifecycleNoDefaultSnapshotSurvivesPoolEdit(t *testing.T) {
	ctx := context.Background()
	claim, pool := independentDefaultFixture()
	c := independentClient(t, claim, pool)
	r := &SandboxClaimReconciler{Client: c, APIReader: c}
	if stopPass, err := r.initializeDefaultLifecycle(ctx, claim); err != nil || stopPass {
		t.Fatalf("initialize: %v %v", stopPass, err)
	}
	saved := independentReadClaim(t, c)
	if saved.Status.DefaultLifecycle == nil || !reflect.DeepEqual(*saved.Status.DefaultLifecycle, ext.Lifecycle{}) {
		t.Fatalf("expected nonnil empty snapshot: %#v", saved.Status.DefaultLifecycle)
	}
	// Pin JSON persistence of {} versus nil. This does NOT prove CRD pruning.
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ext.SandboxClaim
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status.DefaultLifecycle == nil {
		t.Fatal("empty snapshot disappeared in JSON round trip")
	}
	pool.Spec.ClaimDefaults = independentTTL(1)
	if err = c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if stopPass, err := r.initializeDefaultLifecycle(ctx, &decoded); err != nil || stopPass {
		t.Fatalf("reinitialized empty snapshot: %v %v", stopPass, err)
	}
	if expired, _ := r.checkExpiration(&decoded); expired {
		t.Fatal("new pool TTL retroactively expired claim")
	}
}

func TestIndependentDefaultLifecyclePositiveOptIn(t *testing.T) {
	for _, tc := range []struct {
		name         string
		defaults     *ext.SandboxClaimDefaults
		zeroCreation bool
		wantErr      bool
		wantDeadline bool
	}{
		{name: "omitted"}, {name: "empty", defaults: &ext.SandboxClaimDefaults{}},
		{name: "zero rejected defensively", defaults: independentTTL(0), wantErr: true},
		{name: "negative rejected defensively", defaults: independentTTL(-1), wantErr: true},
		{name: "positive", defaults: independentTTL(1), wantDeadline: true},
		{name: "missing creation rejected", defaults: independentTTL(1), zeroCreation: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = tc.defaults
			if tc.zeroCreation {
				claim.CreationTimestamp = metav1.Time{}
			}
			c := independentClient(t, claim, pool)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			_, err := r.initializeDefaultLifecycle(context.Background(), claim)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			got := independentReadClaim(t, c)
			if tc.wantErr {
				if got.Status.DefaultLifecycle != nil {
					t.Fatal("failed resolution persisted a snapshot")
				}
				return
			}
			if got.Status.DefaultLifecycle == nil {
				t.Fatal("decision not persisted")
			}
			if (got.Status.DefaultLifecycle.ShutdownTime != nil) != tc.wantDeadline {
				t.Fatal("unexpected TTL decision")
			}
		})
	}
}

func TestIndependentDefaultLifecycleExplicitPrecedence(t *testing.T) {
	deadline := metav1.NewTime(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, explicit := range []*ext.Lifecycle{{}, {ShutdownTime: &deadline, ShutdownPolicy: ext.ShutdownPolicyDelete}} {
		name := "empty"
		if explicit.ShutdownTime != nil {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			claim, _ := independentDefaultFixture()
			claim.Spec.Lifecycle = explicit
			// No pool exists: an explicit lifecycle must not depend on pool defaults.
			c := independentClient(t, claim)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			if _, err := r.initializeDefaultLifecycle(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			saved := independentReadClaim(t, c)
			if got := claimLifecycle(saved); !apiequality.Semantic.DeepEqual(got, explicit) {
				t.Fatalf("explicit lost precedence: %#v", got)
			}
			if saved.Status.DefaultLifecycle == nil || saved.Status.DefaultLifecycle.ShutdownTime != nil {
				t.Fatal("explicit claim inherited a default")
			}
			// Precedence must also hold when a previous default exists.
			saved.Status.DefaultLifecycle = &ext.Lifecycle{ShutdownTime: &metav1.Time{Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}}
			if !apiequality.Semantic.DeepEqual(claimLifecycle(saved), explicit) {
				t.Fatal("frozen default overrode explicit lifecycle")
			}
		})
	}
}

func TestIndependentDefaultLifecycleLegacyBindingAndMissingPool(t *testing.T) {
	for _, mode := range []string{"status", "annotation", "label", "owned sandbox", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			claim, _ := independentDefaultFixture()
			var extra []client.Object
			switch mode {
			case "status":
				claim.Status.SandboxStatus.Name = "existing"
			case "annotation":
				claim.Annotations = map[string]string{ext.AssignedSandboxNameAnnotation: "existing"}
			case "label":
				claim.Labels = map[string]string{ext.DeprecatedAssignedSandboxNameLabel: "existing"}
			case "owned sandbox":
				controlled := true
				extra = append(extra, &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: ext.GroupVersion.String(), Kind: "SandboxClaim", Name: claim.Name, UID: claim.UID, Controller: &controlled}}}})
			}
			c := independentClient(t, claim, extra...)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			_, err := r.initializeDefaultLifecycle(context.Background(), claim)
			saved := independentReadClaim(t, c)
			if mode == "unbound" {
				if !errors.Is(err, ErrWarmPoolNotFound) {
					t.Fatalf("missing pool error: %v", err)
				}
				if saved.Status.DefaultLifecycle != nil {
					t.Fatal("missing pool must remain retryable")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status.DefaultLifecycle == nil || saved.Status.DefaultLifecycle.ShutdownTime != nil {
				t.Fatal("legacy binding acquired TTL")
			}
		})
	}
}

func TestIndependentDefaultLifecycleAuthoritativeClaimWins(t *testing.T) {
	for _, mode := range []string{"replacement UID", "already initialized"} {
		t.Run(mode, func(t *testing.T) {
			stale, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = independentTTL(60)
			fresh := stale.DeepCopy()
			if mode == "replacement UID" {
				fresh.UID = "replacement-uid"
			} else {
				fresh.Status.DefaultLifecycle = &ext.Lifecycle{}
			}
			c := independentClient(t, fresh, pool)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			_, err := r.initializeDefaultLifecycle(context.Background(), stale)
			if err != nil {
				t.Fatal(err)
			}
			saved := independentReadClaim(t, c)
			if mode == "replacement UID" {
				if saved.UID != "replacement-uid" || saved.Status.DefaultLifecycle != nil {
					t.Fatal("stale reconcile mutated replacement claim")
				}
			} else if saved.Status.DefaultLifecycle == nil || saved.Status.DefaultLifecycle.ShutdownTime != nil {
				t.Fatal("stale reconcile overwrote initialized no-default decision")
			}
		})
	}
}
