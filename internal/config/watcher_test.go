// Copyright 2025 The Kubernetes Authors.
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

package config

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func TestHashData_Deterministic(t *testing.T) {
	d := map[string]string{"b": "2", "a": "1"}
	h1 := hashData(d)
	h2 := hashData(d)
	if h1 != h2 {
		t.Errorf("hashData not deterministic: %s != %s", h1, h2)
	}
}

func TestHashData_Empty(t *testing.T) {
	if hashData(nil) != "empty" {
		t.Error("expected 'empty' for nil data")
	}
	if hashData(map[string]string{}) != "empty" {
		t.Error("expected 'empty' for empty map")
	}
}

func TestHashData_DifferentData(t *testing.T) {
	h1 := hashData(map[string]string{"a": "1"})
	h2 := hashData(map[string]string{"a": "2"})
	if h1 == h2 {
		t.Error("expected different hashes for different data")
	}
}

func TestWatcher_NoChange(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "100"}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "test-ns"},
		Data:       data,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()

	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(data),
	}

	// Should not exit (same hash)
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatcher_IgnoresWrongName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: "empty",
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "other-config", Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatcher_NotFoundMatchesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: "empty",
	}

	// ConfigMap doesn't exist and startup was also empty — no exit
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
