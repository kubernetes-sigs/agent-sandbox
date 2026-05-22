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

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testUID     = types.UID("e2e6c7b8-2222-4444-8888-aaaaaaaaaaaa")
	testUID2    = types.UID("11111111-2222-4444-8888-bbbbbbbbbbbb")
	testPodName = "sandbox-1"
	testPodNS   = "tenants"
	testPodIP   = "10.0.4.42"
	testPodIP2  = "10.0.4.99"
)

// makePod builds a sandbox Pod with the conventional label, an
// OwnerReference to a Sandbox CR with the given UID, a PodIP, and the
// Ready condition status (when ready is true).
func makePod(name, ns string, sandboxUID types.UID, ip string, ready bool) *corev1.Pod {
	tru := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{PodSandboxNameHashLabel: "abc123"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: SandboxAPIGroup + "/v1beta1",
				Kind:       SandboxKind,
				Name:       name,
				UID:        sandboxUID,
				Controller: &tru,
			}},
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
	} else {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		}}
	}
	return pod
}

// waitFor polls until cond returns true or 1s elapses; helper for
// informer-driven async expectations. Tests have not needed a custom
// timeout yet — keep this caller-simple.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func newCache(t *testing.T, objs ...runtime.Object) (*Cache, *fake.Clientset, context.CancelFunc) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	c, err := New(Options{
		Client:    client,
		Log:       logr.Discard(),
		Namespace: "", // cluster-wide
		Resync:    time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	c.Start(ctx)
	if ok := c.WaitForSync(ctx); !ok {
		cancel()
		t.Fatalf("WaitForSync failed")
	}
	return c, client, cancel
}

func TestSandboxUIDOf(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want types.UID
		ok   bool
	}{
		{
			name: "owner is sandbox v1beta1 → uid extracted",
			pod:  makePod("p", "n", testUID, "1.1.1.1", true),
			want: testUID,
			ok:   true,
		},
		{
			name: "owner is sandbox v1alpha1 → also extracted",
			pod: func() *corev1.Pod {
				p := makePod("p", "n", testUID, "1.1.1.1", true)
				p.OwnerReferences[0].APIVersion = SandboxAPIGroup + "/v1alpha1"
				return p
			}(),
			want: testUID,
			ok:   true,
		},
		{
			name: "owner is Deployment → ignored",
			pod: func() *corev1.Pod {
				p := makePod("p", "n", testUID, "1.1.1.1", true)
				p.OwnerReferences[0].Kind = "Deployment"
				p.OwnerReferences[0].APIVersion = "apps/v1"
				return p
			}(),
			ok: false,
		},
		{
			name: "owner from different group → ignored",
			pod: func() *corev1.Pod {
				p := makePod("p", "n", testUID, "1.1.1.1", true)
				p.OwnerReferences[0].APIVersion = "other.example.com/v1"
				return p
			}(),
			ok: false,
		},
		{
			name: "non-controller owner ref → ignored",
			pod: func() *corev1.Pod {
				p := makePod("p", "n", testUID, "1.1.1.1", true)
				f := false
				p.OwnerReferences[0].Controller = &f
				return p
			}(),
			ok: false,
		},
		{
			name: "no owner refs at all → not found",
			pod: func() *corev1.Pod {
				p := makePod("p", "n", testUID, "1.1.1.1", true)
				p.OwnerReferences = nil
				return p
			}(),
			ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sandboxUIDOf(tc.pod)
			if ok != tc.ok {
				t.Fatalf("ok: got %v want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("uid: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestPodReady(t *testing.T) {
	if podReady(&corev1.Pod{}) {
		t.Error("empty pod must not be ready")
	}
	p := makePod("p", "n", testUID, "1.1.1.1", true)
	if !podReady(p) {
		t.Error("ready pod must return true")
	}
	p = makePod("p", "n", testUID, "1.1.1.1", false)
	if podReady(p) {
		t.Error("not-ready pod must return false")
	}
}

func TestCache_PreseededPodCachedOnSync(t *testing.T) {
	pod := makePod(testPodName, testPodNS, testUID, testPodIP, true)
	c, _, cancel := newCache(t, pod)
	defer cancel()

	if !waitFor(t, func() bool { return c.Len() == 1 }) {
		t.Fatalf("expected 1 entry, got %d", c.Len())
	}
	e, ok := c.Get(testUID)
	if !ok {
		t.Fatalf("Get(%q): not found", testUID)
	}
	if e.PodIP != testPodIP || e.SandboxName != testPodName || e.Namespace != testPodNS {
		t.Fatalf("entry mismatch: %+v", e)
	}
}

func TestCache_AddEventCaches(t *testing.T) {
	c, client, cancel := newCache(t)
	defer cancel()

	pod := makePod(testPodName, testPodNS, testUID, testPodIP, true)
	if _, err := client.CoreV1().Pods(testPodNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return ok }) {
		t.Fatalf("Add event was not reflected in cache")
	}
}

func TestCache_NotReadyPodNotCached(t *testing.T) {
	pod := makePod(testPodName, testPodNS, testUID, testPodIP, false)
	c, _, cancel := newCache(t, pod)
	defer cancel()

	// Give the informer a beat to process the initial list.
	time.Sleep(100 * time.Millisecond)
	if _, ok := c.Get(testUID); ok {
		t.Fatalf("not-ready pod must not be cached")
	}
}

func TestCache_FlipsOutWhenPodGoesNotReady(t *testing.T) {
	pod := makePod(testPodName, testPodNS, testUID, testPodIP, true)
	c, client, cancel := newCache(t, pod)
	defer cancel()

	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return ok }) {
		t.Fatalf("initial cache add failed")
	}

	// Update the pod to NotReady.
	pod = pod.DeepCopy()
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := client.CoreV1().Pods(testPodNS).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return !ok }) {
		t.Fatalf("cache entry must be removed when pod flips to NotReady")
	}
}

func TestCache_DeleteRemoves(t *testing.T) {
	pod := makePod(testPodName, testPodNS, testUID, testPodIP, true)
	c, client, cancel := newCache(t, pod)
	defer cancel()

	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return ok }) {
		t.Fatalf("initial cache add failed")
	}
	if err := client.CoreV1().Pods(testPodNS).Delete(t.Context(), testPodName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return !ok }) {
		t.Fatalf("delete event was not reflected in cache")
	}
}

func TestCache_PodIPUpdateRefreshesEntry(t *testing.T) {
	pod := makePod(testPodName, testPodNS, testUID, testPodIP, true)
	c, client, cancel := newCache(t, pod)
	defer cancel()

	if !waitFor(t, func() bool { _, ok := c.Get(testUID); return ok }) {
		t.Fatalf("initial cache add failed")
	}
	pod = pod.DeepCopy()
	pod.Status.PodIP = testPodIP2
	if _, err := client.CoreV1().Pods(testPodNS).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !waitFor(t, func() bool {
		e, ok := c.Get(testUID)
		return ok && e.PodIP == testPodIP2
	}) {
		t.Fatalf("PodIP update was not reflected in cache; current entry: %+v", func() Entry {
			e, _ := c.Get(testUID)
			return e
		}())
	}
}

func TestCache_HandlesMultipleSandboxesIndependently(t *testing.T) {
	a := makePod("a", "ns-a", testUID, "10.0.0.1", true)
	b := makePod("b", "ns-b", testUID2, "10.0.0.2", true)
	c, _, cancel := newCache(t, a, b)
	defer cancel()

	if !waitFor(t, func() bool { return c.Len() == 2 }) {
		t.Fatalf("expected 2 entries, got %d", c.Len())
	}
	if ea, ok := c.Get(testUID); !ok || ea.PodIP != "10.0.0.1" {
		t.Errorf("entry A wrong: %+v ok=%v", ea, ok)
	}
	if eb, ok := c.Get(testUID2); !ok || eb.PodIP != "10.0.0.2" {
		t.Errorf("entry B wrong: %+v ok=%v", eb, ok)
	}
}

func TestApiVersionInGroup(t *testing.T) {
	cases := map[string]bool{
		"agents.x-k8s.io/v1beta1":  true,
		"agents.x-k8s.io/v1alpha1": true,
		"agents.x-k8s.io":          true,
		"apps/v1":                  false,
		"other.example.com/v1":     false,
		"":                         false,
	}
	for in, want := range cases {
		if got := apiVersionInGroup(in, SandboxAPIGroup); got != want {
			t.Errorf("apiVersionInGroup(%q) = %v, want %v", in, got, want)
		}
	}
}
