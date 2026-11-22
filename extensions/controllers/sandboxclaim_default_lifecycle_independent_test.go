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
	if persisted.Status.DefaultShutdownTime == nil || !persisted.Status.DefaultShutdownTime.Time.Equal(want) {
		t.Fatalf("deadline is not creation+TTL: %#v", persisted.Status.DefaultShutdownTime)
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
	if !after.Status.DefaultShutdownTime.Time.Equal(want) {
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

func TestIndependentDefaultLifecycleAssignedClaimDoesNotInheritAfterPoolEdit(t *testing.T) {
	ctx := context.Background()
	claim, pool := independentDefaultFixture()
	claim.Status.SandboxStatus.Name = "already-assigned"
	c := independentClient(t, claim, pool)
	r := &SandboxClaimReconciler{Client: c, APIReader: c}
	if stop, err := r.initializeDefaultLifecycle(ctx, claim); err != nil || stop {
		t.Fatalf("initialize: %v %v", stop, err)
	}
	pool.Spec.ClaimDefaults = independentTTL(1)
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	saved := independentReadClaim(t, c)
	if stop, err := r.initializeDefaultLifecycle(ctx, saved); err != nil || stop {
		t.Fatalf("after edit: %v %v", stop, err)
	}
	if independentReadClaim(t, c).Status.DefaultShutdownTime != nil {
		t.Fatal("TTL was retroactively applied to assigned claim")
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
				if got.Status.DefaultShutdownTime != nil {
					t.Fatal("failed resolution persisted a snapshot")
				}
				return
			}
			if (got.Status.DefaultShutdownTime != nil) != tc.wantDeadline {
				t.Fatal("unexpected TTL decision")
			}
		})
	}
}

func TestIndependentDefaultLifecycleExplicitPrecedence(t *testing.T) {
	deadline := metav1.NewTime(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	zero := int32(0)
	for _, tc := range []struct {
		name        string
		lifecycle   *ext.Lifecycle
		wantDefault bool
	}{
		{"policy only Retain", &ext.Lifecycle{ShutdownPolicy: ext.ShutdownPolicyRetain}, true},
		{"policy only Delete", &ext.Lifecycle{ShutdownPolicy: ext.ShutdownPolicyDelete}, true},
		{"finished TTL", &ext.Lifecycle{TTLSecondsAfterFinished: &zero, ShutdownPolicy: ext.ShutdownPolicyDelete}, true},
		{"explicit deadline", &ext.Lifecycle{ShutdownTime: &deadline, ShutdownPolicy: ext.ShutdownPolicyDelete}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = independentTTL(60)
			claim.Spec.Lifecycle = tc.lifecycle
			c := independentClient(t, claim, pool)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			if stop, err := r.initializeDefaultLifecycle(context.Background(), claim); err != nil || stop {
				t.Fatalf("initialize: %v %v", stop, err)
			}
			saved := independentReadClaim(t, c)
			if (saved.Status.DefaultShutdownTime != nil) != tc.wantDefault {
				t.Fatalf("stored deadline: %v", saved.Status.DefaultShutdownTime)
			}
			if !tc.wantDefault {
				// A later explicit deadline still wins over an already frozen default.
				past := metav1.NewTime(time.Now().Add(-time.Hour))
				saved.Status.DefaultShutdownTime = &past
			}
			want := tc.lifecycle.DeepCopy()
			if tc.wantDefault {
				want.ShutdownTime = saved.Status.DefaultShutdownTime
			}
			if got := claimLifecycle(saved); !apiequality.Semantic.DeepEqual(got, want) {
				t.Fatalf("effective lifecycle: %#v, want %#v", got, want)
			}
			if !apiequality.Semantic.DeepEqual(saved.Spec.Lifecycle, tc.lifecycle) {
				t.Fatal("claim's spec was changed")
			}
		})
	}
}

func TestIndependentDefaultLifecycleLegacyBindingAndMissingPool(t *testing.T) {
	for _, mode := range []string{"status", "annotation", "label", "owned sandbox", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			claim, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = independentTTL(60)
			extra := []client.Object{pool}
			if mode == "unbound" {
				extra = nil
			}
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
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status.DefaultShutdownTime != nil {
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
				deadline := metav1.NewTime(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
				fresh.Status.DefaultShutdownTime = &deadline
			}
			c := independentClient(t, fresh, pool)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			_, err := r.initializeDefaultLifecycle(context.Background(), stale)
			if err != nil {
				t.Fatal(err)
			}
			saved := independentReadClaim(t, c)
			if mode == "replacement UID" {
				if saved.UID != "replacement-uid" || saved.Status.DefaultShutdownTime != nil {
					t.Fatal("stale reconcile mutated replacement claim")
				}
			} else if !saved.Status.DefaultShutdownTime.Equal(fresh.Status.DefaultShutdownTime) {
				t.Fatal("stale reconcile overwrote initialized deadline")
			}
		})
	}
}

func TestPoolShutdownPolicyIsFrozenOnlyForClaimsWithoutLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                      string
		poolPolicy                ext.ShutdownPolicy
		claimLifecycle            *ext.Lifecycle
		wantFrozen, wantEffective ext.ShutdownPolicy
	}{
		{name: "Retain by default"},
		{name: "pool Delete", poolPolicy: ext.ShutdownPolicyDelete, wantFrozen: ext.ShutdownPolicyDelete, wantEffective: ext.ShutdownPolicyDelete},
		{name: "pool DeleteForeground", poolPolicy: ext.ShutdownPolicyDeleteForeground, wantFrozen: ext.ShutdownPolicyDeleteForeground, wantEffective: ext.ShutdownPolicyDeleteForeground},
		{name: "Claim Retain", poolPolicy: ext.ShutdownPolicyDelete, claimLifecycle: &ext.Lifecycle{ShutdownPolicy: ext.ShutdownPolicyRetain}, wantEffective: ext.ShutdownPolicyRetain},
		{name: "Claim Delete", poolPolicy: ext.ShutdownPolicyRetain, claimLifecycle: &ext.Lifecycle{ShutdownPolicy: ext.ShutdownPolicyDelete}, wantEffective: ext.ShutdownPolicyDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = independentTTL(60)
			pool.Spec.ClaimDefaults.ShutdownPolicy = tc.poolPolicy
			claim.Spec.Lifecycle = tc.claimLifecycle
			c := independentClient(t, claim, pool)
			r := &SandboxClaimReconciler{Client: c, APIReader: c}
			if stop, err := r.initializeDefaultLifecycle(ctx, claim); stop || err != nil {
				t.Fatalf("initialize: stop=%v err=%v", stop, err)
			}
			saved := independentReadClaim(t, c)
			if saved.Status.DefaultShutdownTime == nil || saved.Status.DefaultShutdownPolicy != tc.wantFrozen {
				t.Fatalf("unexpected snapshot: %+v", saved.Status)
			}
			if effective := claimLifecycle(saved); effective.ShutdownPolicy != tc.wantEffective {
				t.Fatalf("effective policy=%q want %q", effective.ShutdownPolicy, tc.wantEffective)
			}
			pool.Spec.ClaimDefaults.ShutdownPolicy = ext.ShutdownPolicyDeleteForeground
			if err := c.Update(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if stop, err := r.initializeDefaultLifecycle(ctx, saved); stop || err != nil {
				t.Fatalf("after edit: stop=%v err=%v", stop, err)
			}
			changed := independentReadClaim(t, c)
			if !changed.Status.DefaultShutdownTime.Equal(saved.Status.DefaultShutdownTime) || changed.Status.DefaultShutdownPolicy != tc.wantFrozen {
				t.Fatalf("pool edit changed resolved values: %+v", changed.Status)
			}
			if tc.claimLifecycle != nil {
				changed.Spec.Lifecycle = nil
				if effective := claimLifecycle(changed); effective.ShutdownPolicy != "" {
					t.Fatalf("removing a Claim-specific override exposed destructive pool policy %q", effective.ShutdownPolicy)
				}
			}
		})
	}
}
