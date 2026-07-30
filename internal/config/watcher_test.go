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

func TestHashData_IgnoresDocKeys(t *testing.T) {
	withDoc := map[string]string{
		"_readme":                    "docs only",
		".comment":                   "also ignored",
		"sandbox-concurrent-workers": "100",
	}
	withoutDoc := map[string]string{
		"sandbox-concurrent-workers": "100",
	}
	if hashData(withDoc) != hashData(withoutDoc) {
		t.Error("doc/comment keys must not affect hashData")
	}
	if hashData(map[string]string{"_readme": "only docs"}) != "empty" {
		t.Error("data with only ignored keys should hash as empty")
	}
}

func TestHashData_IgnoresNonTunableFlags(t *testing.T) {
	withZap := map[string]string{
		"zap-log-level":              "debug",
		"sandbox-concurrent-workers": "100",
	}
	withoutZap := map[string]string{
		"sandbox-concurrent-workers": "100",
	}
	if hashData(withZap) != hashData(withoutZap) {
		t.Error("non-tunable flags must not affect hashData")
	}
	if hashData(map[string]string{"zap-log-level": "debug", "leader-elect": "false"}) != "empty" {
		t.Error("data with only non-tunable keys should hash as empty")
	}
}

func TestHashData_IncludesUnknownKeys(t *testing.T) {
	// Unknown mounted keys (e.g. allowed-label-domains) are not flag overrides
	// but still affect runtime behavior via other readers — hashing must include them.
	base := map[string]string{"sandbox-concurrent-workers": "100"}
	withUnknown := map[string]string{
		"sandbox-concurrent-workers": "100",
		"allowed-label-domains":      "example.com",
	}
	if hashData(base) == hashData(withUnknown) {
		t.Error("unknown ConfigMap keys must affect hashData")
	}
}

func TestWatcher_NoChange(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "100"}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "test-ns"},
		Data:       data,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(data),
		Shutdown:    func() { shutdownCalled = true },
	}

	// Should not exit (same hash)
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("Shutdown must not be called when ConfigMap is unchanged")
	}
}

func TestWatcher_IgnoresWrongName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: "empty",
		Shutdown:    func() { shutdownCalled = true },
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "other-config", Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("Shutdown must not be called for unrelated ConfigMaps")
	}
}

func TestWatcher_NotFoundMatchesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: "empty",
		Shutdown:    func() { shutdownCalled = true },
	}

	// ConfigMap doesn't exist and startup was also empty — no exit
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("Shutdown must not be called when missing ConfigMap matches empty startup")
	}
}

func TestWatcher_ChangeTriggersShutdown(t *testing.T) {
	startup := map[string]string{"sandbox-concurrent-workers": "100"}
	updated := map[string]string{"sandbox-concurrent-workers": "200"}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "test-ns"},
		Data:       updated,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(startup),
		Shutdown:    func() { shutdownCalled = true },
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shutdownCalled {
		t.Error("Shutdown must be called when ConfigMap data changes")
	}
}

func TestWatcher_DeleteTriggersShutdown(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(map[string]string{"sandbox-concurrent-workers": "100"}),
		Shutdown:    func() { shutdownCalled = true },
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shutdownCalled {
		t.Error("Shutdown must be called when a previously present ConfigMap is deleted")
	}
}

func TestWatcher_DocKeyOnlyChangeDoesNotShutdown(t *testing.T) {
	startup := map[string]string{
		"_readme":                    "old docs",
		"sandbox-concurrent-workers": "100",
	}
	updated := map[string]string{
		"_readme":                    "new docs",
		"sandbox-concurrent-workers": "100",
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "test-ns"},
		Data:       updated,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(startup),
		Shutdown:    func() { shutdownCalled = true },
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("Shutdown must not be called when only ignored doc keys change")
	}
}

func TestWatcher_NonTunableOnlyChangeDoesNotShutdown(t *testing.T) {
	startup := map[string]string{
		"zap-log-level":              "info",
		"sandbox-concurrent-workers": "100",
	}
	updated := map[string]string{
		"zap-log-level":              "debug",
		"sandbox-concurrent-workers": "100",
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "test-ns"},
		Data:       updated,
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()

	var shutdownCalled bool
	w := &MapWatcher{
		Client:      c,
		Namespace:   "test-ns",
		StartupHash: hashData(startup),
		Shutdown:    func() { shutdownCalled = true },
	}

	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: configMapName, Namespace: "test-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdownCalled {
		t.Error("Shutdown must not be called when only non-tunable flags change")
	}
}
