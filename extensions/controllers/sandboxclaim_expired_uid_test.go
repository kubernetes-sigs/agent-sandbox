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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// This asserts the delete request's UID guard, not API-server enforcement or a race.
func TestIndependentExpiredSandboxDeleteUIDPrecondition(t *testing.T) {
	ctx := context.Background()
	controller := true
	claim := &ext.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"}}
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: claim.Name, Namespace: claim.Namespace, UID: "observed-sandbox-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: ext.GroupVersion.String(), Kind: "SandboxClaim", Name: claim.Name, UID: claim.UID, Controller: &controller}},
	}}
	calls := 0
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sandbox).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			calls++
			options := &client.DeleteOptions{}
			for _, opt := range opts {
				opt.ApplyToDelete(options)
			}
			if obj.GetUID() != sandbox.UID {
				t.Errorf("deleted object UID = %q, want %q", obj.GetUID(), sandbox.UID)
			}
			if options.Preconditions == nil || options.Preconditions.UID == nil {
				t.Error("delete omitted UID precondition")
			} else if *options.Preconditions.UID != sandbox.UID {
				t.Errorf("delete precondition UID = %q, want %q", *options.Preconditions.UID, sandbox.UID)
			}
			return nil
		},
	}).Build()
	r := &SandboxClaimReconciler{Client: c}
	got, err := r.reconcileExpired(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.UID != sandbox.UID {
		t.Fatalf("returned sandbox = %#v", got)
	}
	if calls != 1 {
		t.Fatalf("delete calls = %d, want 1", calls)
	}
}
