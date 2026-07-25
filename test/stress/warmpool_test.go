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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// poolSandboxObj builds an unstructured Sandbox owned by the given pool
// (pool "" = no SandboxWarmPool owner reference).
func poolSandboxObj(namespace, name, pool string, uid types.UID) *unstructured.Unstructured {
	metadata := map[string]any{
		"name":      name,
		"namespace": namespace,
		"uid":       string(uid),
	}
	if pool != "" {
		metadata["ownerReferences"] = []any{
			map[string]any{
				"apiVersion": extensionsGroupVersion,
				"kind":       "SandboxWarmPool",
				"name":       pool,
				"uid":        "owner-" + pool,
			},
		}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agents.x-k8s.io/v1beta1",
			"kind":       "Sandbox",
			"metadata":   metadata,
		},
	}
}

func TestPoolAccountant(t *testing.T) {
	a := newPoolAccountant("ns", []string{"pool-a", "pool-b"}, 3)

	add := func(pool string, uid types.UID) {
		a.observe("sandboxes", watch.Added, poolSandboxObj("ns", pool+"-"+string(uid), pool, uid))
	}
	del := func(pool string, uid types.UID) {
		a.observe("sandboxes", watch.Deleted, poolSandboxObj("ns", pool+"-"+string(uid), pool, uid))
	}

	// Initial fill of pool-a: exactly 3 distinct creates, no violations.
	add("pool-a", "a1")
	add("pool-a", "a2")
	add("pool-a", "a3")
	// Duplicate events for a known UID (e.g. MODIFIED updates) are not creates.
	a.observe("sandboxes", watch.Modified, poolSandboxObj("ns", "pool-a-a1", "pool-a", "a1"))

	// A legitimate replacement: delete observed BEFORE the extra create.
	del("pool-a", "a2")
	add("pool-a", "a4")

	// An over-create: a 4th live member with no delete credit.
	add("pool-a", "a5")

	// pool-b: first sighting via MODIFIED (re-established watch) counts as a
	// create; events in other namespaces or without a pool owner are ignored.
	a.observe("sandboxes", watch.Modified, poolSandboxObj("ns", "pool-b-b1", "pool-b", "b1"))
	a.observe("sandboxes", watch.Added, poolSandboxObj("other-ns", "pool-b-x", "pool-b", "x1"))
	a.observe("sandboxes", watch.Added, poolSandboxObj("ns", "orphan", "", "o1"))
	a.observe("sandboxes", watch.Added, poolSandboxObj("ns", "unknown-pool-sb", "pool-c", "c1"))
	a.observe("pods", watch.Added, poolSandboxObj("ns", "pool-b-b9", "pool-b", "b9"))
	// A delete for a UID never seen live is ignored (no phantom credit).
	del("pool-b", "b7")
	add("pool-b", "b2")
	add("pool-b", "b3")
	add("pool-b", "b4") // over-create: no delete was observed in pool-b

	totals := a.totals()
	if got, want := totals.distinctCreates, 5+4; got != want {
		t.Errorf("distinctCreates = %d, want %d", got, want)
	}
	if got, want := totals.replacements, 1; got != want {
		t.Errorf("replacements = %d, want %d", got, want)
	}
	if got, want := totals.overCreates, 2; got != want {
		t.Errorf("overCreates = %d, want %d", got, want)
	}
	if got, want := totals.deletes, 1; got != want {
		t.Errorf("deletes = %d, want %d", got, want)
	}
	// pool-a live peaked at 4 ({a1,a3,a4,a5}); pool-b at 4 ({b1..b4}).
	if got, want := totals.maxPoolPeakLive, 4; got != want {
		t.Errorf("maxPoolPeakLive = %d, want %d", got, want)
	}
	if got, want := totals.poolsExceedingTarget, 2; got != want {
		t.Errorf("poolsExceedingTarget = %d, want %d", got, want)
	}
	// Global concurrent peak: pool-a contributes 4 live and pool-b 4 live at
	// the end; the peak is reached at the final add (8 concurrent).
	if got, want := totals.peakLiveAll, 8; got != want {
		t.Errorf("peakLiveAll = %d, want %d", got, want)
	}
	if len(totals.worstPools) != 2 || !strings.Contains(totals.worstPools[0], "pool-a") {
		t.Errorf("worstPools = %v, want both pools flagged", totals.worstPools)
	}
}

func TestPoolAccountantCleanShape(t *testing.T) {
	// The PASS shape: exactly replicas creates per pool, nothing else.
	a := newPoolAccountant("ns", []string{"p"}, 2)
	a.observe("sandboxes", watch.Added, poolSandboxObj("ns", "p-1", "p", "u1"))
	a.observe("sandboxes", watch.Added, poolSandboxObj("ns", "p-2", "p", "u2"))
	totals := a.totals()
	if totals.distinctCreates != 2 || totals.replacements != 0 || totals.overCreates != 0 || totals.deletes != 0 {
		t.Errorf("unexpected totals for clean shape: %+v", totals)
	}
	if totals.maxPoolPeakLive != 2 || totals.poolsExceedingTarget != 0 || totals.peakLiveAll != 2 {
		t.Errorf("unexpected peaks for clean shape: %+v", totals)
	}
}

// poolEvent builds an unstructured core-v1 Event for a SandboxWarmPool.
func poolEvent(namespace, pool, reason, eventType string, uid types.UID, count int64) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]any{
			"name":      "evt-" + string(uid),
			"namespace": namespace,
			"uid":       string(uid),
		},
		"reason": reason,
		"type":   eventType,
		"involvedObject": map[string]any{
			"kind":      "SandboxWarmPool",
			"name":      pool,
			"namespace": namespace,
		},
	}
	if count > 0 {
		obj["count"] = count
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestWarmPoolEventCounter(t *testing.T) {
	c := newWarmPoolEventCounter("ns", "pool", warmPoolNotProgressingReason)

	if n, first := c.occurrences(); n != 0 || !first.IsZero() {
		t.Fatalf("occurrences before any event = %d, firstSeen zero = %v", n, first.IsZero())
	}

	// Non-matching events are ignored: wrong reason, type, kind, pool,
	// namespace, resource, and DELETED events.
	c.observe("events", watch.Added, poolEvent("ns", "pool", "WarmPoolProgressing", "Normal", "e0", 0))
	c.observe("events", watch.Added, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Normal", "e1", 0))
	c.observe("events", watch.Added, poolEvent("ns", "other", warmPoolNotProgressingReason, "Warning", "e2", 0))
	c.observe("events", watch.Added, poolEvent("other", "pool", warmPoolNotProgressingReason, "Warning", "e3", 0))
	c.observe("pods", watch.Added, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "e4", 0))
	c.observe("events", watch.Deleted, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "e5", 0))
	notPool := poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "e6", 0)
	_ = unstructured.SetNestedField(notPool.Object, "Sandbox", "involvedObject", "kind")
	c.observe("events", watch.Added, notPool)
	if n, _ := c.occurrences(); n != 0 {
		t.Fatalf("occurrences after non-matching events = %d, want 0", n)
	}

	// One matching event without a count field counts once.
	c.observe("events", watch.Added, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "evt1", 0))
	if n, first := c.occurrences(); n != 1 || first.IsZero() {
		t.Fatalf("occurrences = %d (firstSeen zero=%v), want 1 with firstSeen set", n, first.IsZero())
	}

	// A dedup bump on the SAME event object (count=3) raises occurrences to
	// 3: a re-emit folded into one object still fails exactly-once.
	c.observe("events", watch.Modified, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "evt1", 3))
	if n, _ := c.occurrences(); n != 3 {
		t.Fatalf("occurrences after count bump = %d, want 3", n)
	}
	// Stale re-delivery with a lower count does not decrease.
	c.observe("events", watch.Modified, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "evt1", 2))
	if n, _ := c.occurrences(); n != 3 {
		t.Fatalf("occurrences after stale re-delivery = %d, want 3", n)
	}

	// A second distinct event object adds its own occurrence.
	c.observe("events", watch.Added, poolEvent("ns", "pool", warmPoolNotProgressingReason, "Warning", "evt2", 0))
	if n, _ := c.occurrences(); n != 4 {
		t.Fatalf("occurrences after second event object = %d, want 4", n)
	}
}

func TestBuildUnschedulableTemplateObject(t *testing.T) {
	id := types.NamespacedName{Name: "tmpl", Namespace: "ns"}
	obj := buildUnschedulableTemplateObject(id, "debian:latest", "1000")

	// Direct map access: the unstructured helpers deep-copy via DeepCopyJSON,
	// which rejects the template's []string command value.
	containers := obj.Object["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(containers))
	}
	container := containers[0].(map[string]any)
	cpu, found, err := unstructured.NestedString(container, "resources", "requests", "cpu")
	if err != nil || !found {
		t.Fatalf("cpu request not found: found=%v err=%v", found, err)
	}
	if q, err := resource.ParseQuantity(cpu); err != nil || q.Value() != 1000 {
		t.Errorf("cpu request %q: parse err=%v value=%v, want 1000 cores", cpu, err, q.Value())
	}
	// The rest of the template must keep the shared shape (service-free).
	service, found, _ := unstructured.NestedBool(obj.Object, "spec", "service")
	if !found || service {
		t.Errorf("spec.service = %v (found=%v), want explicit false", service, found)
	}
}

func TestParseWarmPoolPhases(t *testing.T) {
	valid := Config{
		WPPools:                20,
		WPReplicas:             25,
		WPReplacementTolerance: 2,
		WPUnschedReplicas:      3,
		WPUnschedCPU:           "1000",
		WPUnschedWatch:         8 * time.Minute,
		CreateConcurrency:      20,
	}

	phases, err := parsePhases([]string{string(PhaseWarmPoolOvercreate), string(PhaseWarmPoolUnschedulable)}, valid)
	if err != nil {
		t.Fatalf("parsePhases(valid) error: %v", err)
	}
	if len(phases) != 2 {
		t.Fatalf("parsePhases returned %d phases, want 2", len(phases))
	}
	oc, ok := phases[0].(*warmPoolOvercreatePhase)
	if !ok || oc.Name() != PhaseWarmPoolOvercreate || oc.Kind() != PhaseWarmPoolOvercreate {
		t.Fatalf("phases[0] = %#v, want warmPoolOvercreatePhase", phases[0])
	}
	un, ok := phases[1].(*warmPoolUnschedulablePhase)
	if !ok || un.Name() != PhaseWarmPoolUnschedulable || un.Kind() != PhaseWarmPoolUnschedulable {
		t.Fatalf("phases[1] = %#v, want warmPoolUnschedulablePhase", phases[1])
	}

	// Labeled entries keep distinct names but report the base kind.
	labeled, err := parsePhase("warmpool-overcreate-label:x2")
	if err != nil {
		t.Fatalf("parsePhase(labeled) error: %v", err)
	}
	if labeled.Name() != "warmpool-overcreate-label:x2" || labeled.Kind() != PhaseWarmPoolOvercreate {
		t.Errorf("labeled Name/Kind = %q/%q", labeled.Name(), labeled.Kind())
	}
	if _, err := parsePhase("warmpool-overcreate-pct:80"); err == nil {
		t.Errorf("parsePhase accepted a fill-only argument on warmpool-overcreate")
	}

	// Resolve sizes the phases: overcreate budgets pools*replicas pods at
	// peak and drains before the phase ends; unschedulable consumes none.
	info := &ClusterInfo{Nodes: 12, PodCapacity: 1320, PreexistingPods: 28}
	after, peak := oc.Resolve(valid, info, 28)
	if after != 28 || peak != 28+500 {
		t.Errorf("overcreate Resolve = (%d, %d), want (28, 528)", after, peak)
	}
	if oc.Requested() != 500 {
		t.Errorf("overcreate Requested = %d, want 500", oc.Requested())
	}
	after, peak = un.Resolve(valid, info, 28)
	if after != 28 || peak != 28 {
		t.Errorf("unschedulable Resolve = (%d, %d), want (28, 28)", after, peak)
	}
	if un.Requested() != 3 {
		t.Errorf("unschedulable Requested = %d, want 3", un.Requested())
	}

	invalid := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero pools", func(c *Config) { c.WPPools = 0 }},
		{"zero replicas", func(c *Config) { c.WPReplicas = 0 }},
		{"negative tolerance", func(c *Config) { c.WPReplacementTolerance = -1 }},
		{"zero unsched replicas", func(c *Config) { c.WPUnschedReplicas = 0 }},
		{"zero watch window", func(c *Config) { c.WPUnschedWatch = 0 }},
		{"bad cpu quantity", func(c *Config) { c.WPUnschedCPU = "a-lot" }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := parsePhases([]string{string(PhaseWarmPoolOvercreate), string(PhaseWarmPoolUnschedulable)}, cfg); err == nil {
				t.Errorf("parsePhases accepted invalid config (%s)", tc.name)
			}
		})
	}
}

func TestAttachWarmPoolReports(t *testing.T) {
	s := &stressTest{}
	oc := &WarmPoolOvercreateReport{Pools: 20, ReplicasPerPool: 25, TargetSandboxes: 500}
	un := &WarmPoolUnschedulableReport{Replicas: 3, NotProgressingEvents: 1}
	s.setWarmPoolOvercreateReport(1, oc)
	s.setWarmPoolUnschedulableReport(2, un)

	summary := &Summary{Phases: []*PhaseSummary{
		{Number: 1, Name: string(PhaseWarmPoolOvercreate)},
		{Number: 2, Name: string(PhaseWarmPoolUnschedulable)},
		{Number: 3, Name: string(PhaseProbe)},
	}}
	s.attachWarmPoolReports(summary)

	if summary.Phases[0].WarmPoolOvercreate != oc {
		t.Errorf("phase 1 missing overcreate report")
	}
	if summary.Phases[1].WarmPoolUnschedulable != un {
		t.Errorf("phase 2 missing unschedulable report")
	}
	if summary.Phases[2].WarmPoolOvercreate != nil || summary.Phases[2].WarmPoolUnschedulable != nil {
		t.Errorf("phase 3 unexpectedly got warm pool reports")
	}
}
