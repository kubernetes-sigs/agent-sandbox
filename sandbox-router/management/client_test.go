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

package management

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// newTestClient builds a *Client backed by the controller-runtime fake client.
// Access to unexported fields is allowed because this file is in package management.
func newTestClient(t *testing.T, objs ...runtimeclient.Object) *Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := extensionsv1beta1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Client{client: fc, defaultNamespace: "default"}
}

func TestCreate_WithoutKey(t *testing.T) {
	mc := newTestClient(t)
	claim, err := mc.Create(context.Background(), &CreateSandboxRequest{WarmPool: "pool-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := claim.Labels[idempotencyKeyLabel]; ok {
		t.Error("expected no idempotency label on claim created without a key")
	}
}

func TestCreate_WithKey_StampsLabel(t *testing.T) {
	mc := newTestClient(t)
	const key = "test-key-abc-123"
	claim, err := mc.Create(context.Background(), &CreateSandboxRequest{
		WarmPool:       "pool-a",
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := claim.Labels[idempotencyKeyLabel]; got != key {
		t.Errorf("label %q = %q, want %q", idempotencyKeyLabel, got, key)
	}
}

func TestCreate_WithKey_Idempotent(t *testing.T) {
	mc := newTestClient(t)
	req := &CreateSandboxRequest{WarmPool: "pool-a", IdempotencyKey: "idempotency-key-xyz"}

	first, err := mc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	second, err := mc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}

	if first.Name != second.Name {
		t.Errorf("idempotent create returned different names: first=%q second=%q", first.Name, second.Name)
	}

	// Only one claim should exist in the store.
	list := &extensionsv1beta1.SandboxClaimList{}
	if err := mc.client.List(context.Background(), list, runtimeclient.InNamespace("default")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := len(list.Items); n != 1 {
		t.Errorf("expected 1 claim in store, got %d", n)
	}
}

func TestCreate_DifferentKeys_CreateSeparateClaims(t *testing.T) {
	mc := newTestClient(t)

	_, err := mc.Create(context.Background(), &CreateSandboxRequest{WarmPool: "pool-a", IdempotencyKey: "key-one"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = mc.Create(context.Background(), &CreateSandboxRequest{WarmPool: "pool-a", IdempotencyKey: "key-two"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}

	list := &extensionsv1beta1.SandboxClaimList{}
	if err := mc.client.List(context.Background(), list, runtimeclient.InNamespace("default")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := len(list.Items); n != 2 {
		t.Errorf("expected 2 claims in store, got %d", n)
	}
}

func TestCreate_KeyIsolatedByNamespace(t *testing.T) {
	mc := newTestClient(t)
	const key = "shared-key"

	// Create in namespace "ns-a".
	claimA, err := mc.Create(context.Background(), &CreateSandboxRequest{
		WarmPool: "pool-a", Namespace: "ns-a", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create ns-a: %v", err)
	}

	// Same key in a different namespace must create a new claim.
	claimB, err := mc.Create(context.Background(), &CreateSandboxRequest{
		WarmPool: "pool-a", Namespace: "ns-b", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create ns-b: %v", err)
	}

	if claimA.Name == claimB.Name {
		t.Error("same key in different namespaces should produce distinct claims")
	}
}
