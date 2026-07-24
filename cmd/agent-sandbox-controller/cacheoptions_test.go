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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"sigs.k8s.io/agent-sandbox/controllers"
)

// assertStripsManagedFields runs the configured DefaultTransform against an
// object carrying managedFields and asserts they come out cleared — presence
// of SOME transform is not enough, it must be the managedFields strip.
func assertStripsManagedFields(t *testing.T, opts cache.Options) {
	t.Helper()
	if opts.DefaultTransform == nil {
		t.Fatal("DefaultTransform (managedFields strip) not set")
	}
	in := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "cm",
		ManagedFields: []metav1.ManagedFieldsEntry{{
			Manager: "kubelet", Operation: metav1.ManagedFieldsOperationApply,
		}},
	}}
	out, err := opts.DefaultTransform(in)
	if err != nil {
		t.Fatalf("DefaultTransform: %v", err)
	}
	cm, ok := out.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("DefaultTransform returned %T, want *corev1.ConfigMap", out)
	}
	if len(cm.GetManagedFields()) != 0 {
		t.Errorf("DefaultTransform left %d managedFields entries, want 0", len(cm.GetManagedFields()))
	}
}

// entriesByType splits the ByObject map into the per-type entries, failing on
// duplicates: ByObject is keyed by pointer, so an accidental second
// &corev1.Pod{} key would silently produce two Pod configurations.
func entriesByType(t *testing.T, opts cache.Options) (pod, svc *cache.ByObject) {
	t.Helper()
	for obj, entry := range opts.ByObject {
		e := entry
		switch obj.(type) {
		case *corev1.Pod:
			if pod != nil {
				t.Fatal("duplicate *corev1.Pod entries in ByObject")
			}
			pod = &e
		case *corev1.Service:
			if svc != nil {
				t.Fatal("duplicate *corev1.Service entries in ByObject")
			}
			svc = &e
		default:
			t.Fatalf("unexpected ByObject key type %T", obj)
		}
	}
	return pod, svc
}

func TestBuildCacheOptionsUnscoped(t *testing.T) {
	opts, err := buildCacheOptions(false)
	if err != nil {
		t.Fatalf("buildCacheOptions(false): %v", err)
	}
	assertStripsManagedFields(t, opts)
	pod, svc := entriesByType(t, opts)
	if pod == nil {
		t.Fatal("no Pod entry in ByObject")
	}
	if pod.Transform == nil {
		t.Error("Pod entry lost PodCacheTransform")
	}
	if pod.Label != nil {
		t.Errorf("Pod cache unexpectedly label-scoped without the flag: %v", pod.Label)
	}
	if svc != nil {
		t.Errorf("Service entry present without the flag: %+v", svc)
	}
}

func TestBuildCacheOptionsScopedToTrackingLabel(t *testing.T) {
	opts, err := buildCacheOptions(true)
	if err != nil {
		t.Fatalf("buildCacheOptions(true): %v", err)
	}
	assertStripsManagedFields(t, opts)
	pod, svc := entriesByType(t, opts)
	if pod == nil {
		t.Fatal("no Pod entry in ByObject")
	}
	if pod.Transform == nil {
		t.Error("scoping the Pod cache must retain PodCacheTransform")
	}
	if svc == nil {
		t.Fatal("no Service entry in ByObject with the flag enabled")
	}
	want := controllers.SandboxNameHashLabel // selector: label exists
	for name, entry := range map[string]*cache.ByObject{"Pod": pod, "Service": svc} {
		if entry.Label == nil {
			t.Errorf("%s cache not label-scoped", name)
			continue
		}
		if got := entry.Label.String(); got != want {
			t.Errorf("%s cache selector = %q, want %q", name, got, want)
		}
	}
}
