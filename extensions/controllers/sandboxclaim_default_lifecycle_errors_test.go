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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type independentFailStatusClient struct {
	client.Client
	failure error
	payload []byte
}

func (c *independentFailStatusClient) Status() client.SubResourceWriter {
	return &independentFailStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type independentFailStatusWriter struct {
	client.SubResourceWriter
	parent *independentFailStatusClient
}

func (w *independentFailStatusWriter) Patch(_ context.Context, obj client.Object, p client.Patch, _ ...client.SubResourcePatchOption) error {
	var err error
	w.parent.payload, err = p.Data(obj)
	if err != nil {
		return err
	}
	return w.parent.failure
}

type independentFailReader struct {
	client.Reader
	failure error
}

func (r independentFailReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.failure
}

func TestIndependentDefaultLifecycleStatusFailureFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"conflict", apierrors.NewConflict(schema.GroupResource{Group: ext.GroupVersion.Group, Resource: "sandboxclaims"}, "claim", errors.New("concurrent writer"))},
		{"transport", errors.New("status endpoint unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			claim, pool := independentDefaultFixture()
			pool.Spec.ClaimDefaults = independentTTL(60)
			base := independentClient(t, claim, pool)
			failing := &independentFailStatusClient{Client: base, failure: tc.failure}
			r := &SandboxClaimReconciler{Client: failing, APIReader: base}
			live := independentReadClaim(t, base)
			stopPass, err := r.initializeDefaultLifecycle(ctx, live)
			if apierrors.IsConflict(tc.failure) && (err != nil || !stopPass) ||
				!apierrors.IsConflict(tc.failure) && (!errors.Is(err, tc.failure) || stopPass) {
				t.Fatalf("failed write permitted progress: stopPass=%v err=%v", stopPass, err)
			}
			persisted := independentReadClaim(t, base)
			if persisted.Status.DefaultShutdownTime != nil {
				t.Fatal("failed status write persisted a decision")
			}
			var payload struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err = json.Unmarshal(failing.payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Metadata.ResourceVersion == "" {
				t.Fatal("status patch omitted optimistic resourceVersion precondition")
			}
			var sandboxes sandboxv1beta1.SandboxList
			if err = base.List(ctx, &sandboxes); err != nil {
				t.Fatal(err)
			}
			if len(sandboxes.Items) != 0 {
				t.Fatal("initializer allocated sandbox before durable decision")
			}
			// Retry with a newly fetched claim, as a new reconciliation would. This
			// does not simulate API conflict resolution or watch delivery.
			good := &SandboxClaimReconciler{Client: base, APIReader: base}
			if stopPass, err = good.initializeDefaultLifecycle(ctx, persisted); err != nil || stopPass {
				t.Fatalf("retry failed: %v %v", stopPass, err)
			}
		})
	}
}

func TestIndependentDefaultLifecycleAuthoritativeReadFailure(t *testing.T) {
	claim, pool := independentDefaultFixture()
	pool.Spec.ClaimDefaults = independentTTL(60)
	base := independentClient(t, claim, pool)
	failure := errors.New("API reader unavailable")
	r := &SandboxClaimReconciler{Client: base, APIReader: independentFailReader{Reader: base, failure: failure}}
	stopPass, err := r.initializeDefaultLifecycle(context.Background(), claim)
	if stopPass || !errors.Is(err, failure) {
		t.Fatalf("read failure did not stop initialization: %v %v", stopPass, err)
	}
	if independentReadClaim(t, base).Status.DefaultShutdownTime != nil {
		t.Fatal("read failure persisted a default")
	}
}

func TestDefaultDeadlineFailedAttemptDoesNotFreezePreviousPoolAfterClaimUpdate(t *testing.T) {
	ctx := context.Background()
	claim, originalPool := independentDefaultFixture()
	originalPool.Spec.ClaimDefaults = independentTTL(60)
	nextPool := originalPool.DeepCopy()
	nextPool.Name = "replacement-pool"
	nextPool.Spec.ClaimDefaults = independentTTL(3600)
	base := independentClient(t, claim, originalPool, nextPool)
	ackLost := errors.New("status response unavailable")
	failing := &independentFailStatusClient{Client: base, failure: ackLost}
	r := &SandboxClaimReconciler{Client: failing, APIReader: base}
	stale := independentReadClaim(t, base)
	if stop, err := r.initializeDefaultLifecycle(ctx, stale.DeepCopy()); stop || !errors.Is(err, ackLost) {
		t.Fatalf("unexpected initial attempt result: stop=%v err=%v", stop, err)
	}
	changed := independentReadClaim(t, base)
	if changed.Status.DefaultShutdownTime != nil {
		t.Fatal("test did not simulate an uncommitted status PATCH")
	}
	changed.Spec.WarmPoolRef.Name = nextPool.Name
	if err := base.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	r.Client = base
	if stop, err := r.initializeDefaultLifecycle(ctx, stale); stop || err != nil {
		t.Fatalf("resolve replacement pool: stop=%v err=%v", stop, err)
	}
	persisted := independentReadClaim(t, base)
	want := persisted.CreationTimestamp.Add(time.Hour)
	if persisted.Status.DefaultShutdownTime == nil || !persisted.Status.DefaultShutdownTime.Time.Equal(want) {
		t.Fatalf("deadline=%v want %v from replacement pool", persisted.Status.DefaultShutdownTime, want)
	}
}
