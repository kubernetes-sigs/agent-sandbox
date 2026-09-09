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
)

func TestParsePhase(t *testing.T) {
	tests := []struct {
		raw string
		// want is a non-nil prototype whose type and fields the parsed
		// phase must match; nil means parsing must fail.
		want Phase
		// name overrides the expected Name() (defaults to raw).
		name PhaseName
	}{
		{raw: "fill", want: &fillPhase{raw: "fill"}},
		{raw: "fill-pct:80", want: &fillPhase{raw: "fill-pct:80", pct: 80}},
		{raw: "fill-pct:1", want: &fillPhase{raw: "fill-pct:1", pct: 1}},
		{raw: "fill-pct:100", want: &fillPhase{raw: "fill-pct:100", pct: 100}},
		{raw: "fill-label:second", want: &fillPhase{raw: "fill-label:second"}},
		{raw: "probe", want: &probePhase{raw: "probe"}},
		{raw: "claims-warm", want: &claimsWarmPhase{raw: "claims-warm"}},
		{raw: "claims-warm-sustained", want: &claimsWarmSustainedPhase{raw: "claims-warm-sustained"}},
		{raw: "throughput", want: &throughputPhase{raw: "throughput-mif50", mif: 50}, name: "throughput-mif50"},
		{raw: "throughput-mif:400", want: &throughputPhase{raw: "throughput-mif:400", mif: 400}},
		{raw: "throughput-mif:400-label:hot", want: &throughputPhase{raw: "throughput-mif:400-label:hot", mif: 400}},
		// Legacy spelling, predates the key:value form.
		{raw: "throughput-mif600", want: &throughputPhase{raw: "throughput-mif600", mif: 600}},

		{raw: ""},
		{raw: "warmup"},
		{raw: "fil"},
		{raw: "fill-pct:0"},
		{raw: "fill-pct:101"},
		{raw: "fill-pct:abc"},
		{raw: "fill-pct"},                     // missing :value
		{raw: "fill-util80"},                  // pre-review spelling, no longer accepted
		{raw: "throughput-mif"},               // legacy form without a number
		{raw: "throughput-mif0"},              // mif must be positive
		{raw: "throughput-label:x"},           // mif is required
		{raw: "throughput-mif:400-mif:500"},   // duplicate key
		{raw: "throughput-mif:400-label:HOT"}, // labels are [a-z0-9_.]
		{raw: "throughput-mif:400-flavor:x"},  // unknown argument
		{raw: "throughput-mif400-full"},       // labels need the key:value form
		{raw: "claims-warm-sustained-pct:80"}, // pct is fill-only
	}
	for _, tc := range tests {
		got, err := parsePhase(tc.raw)
		if tc.want == nil {
			if err == nil {
				t.Errorf("parsePhase(%q) = %#v, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePhase(%q) returned error: %v", tc.raw, err)
			continue
		}
		wantName := tc.name
		if wantName == "" {
			wantName = PhaseName(tc.raw)
		}
		if got.Name() != wantName {
			t.Errorf("parsePhase(%q).Name() = %q, want %q", tc.raw, got.Name(), wantName)
		}
		switch want := tc.want.(type) {
		case *fillPhase:
			p, ok := got.(*fillPhase)
			if !ok || p.pct != want.pct {
				t.Errorf("parsePhase(%q) = %#v, want fillPhase pct=%d", tc.raw, got, want.pct)
			}
		case *throughputPhase:
			p, ok := got.(*throughputPhase)
			if !ok || p.mif != want.mif {
				t.Errorf("parsePhase(%q) = %#v, want throughputPhase mif=%d", tc.raw, got, want.mif)
			}
		case *probePhase:
			if _, ok := got.(*probePhase); !ok {
				t.Errorf("parsePhase(%q) = %#v, want probePhase", tc.raw, got)
			}
		case *claimsWarmPhase:
			if _, ok := got.(*claimsWarmPhase); !ok {
				t.Errorf("parsePhase(%q) = %#v, want claimsWarmPhase", tc.raw, got)
			}
		case *claimsWarmSustainedPhase:
			if _, ok := got.(*claimsWarmSustainedPhase); !ok {
				t.Errorf("parsePhase(%q) = %#v, want claimsWarmSustainedPhase", tc.raw, got)
			}
		}
	}
}

func TestPhaseKind(t *testing.T) {
	// Kind must ignore arguments: labeled phases still select their
	// kind-specific output in summary.json and printReport.
	tests := map[string]PhaseName{
		"fill":                         PhaseFill,
		"fill-pct:80":                  PhaseFill,
		"probe-label:x":                PhaseProbe,
		"throughput-mif600":            PhaseThroughput,
		"throughput-mif:400-label:hot": PhaseThroughput,
		"claims-warm-label:x":          PhaseClaimsWarm,
		"claims-warm-sustained":        PhaseClaimsWarmSustained,
	}
	for raw, want := range tests {
		p, err := parsePhase(raw)
		if err != nil {
			t.Errorf("parsePhase(%q): %v", raw, err)
			continue
		}
		if got := p.Kind(); got != want {
			t.Errorf("parsePhase(%q).Kind() = %q, want %q", raw, got, want)
		}
	}
}

func TestFileSafePhase(t *testing.T) {
	tests := map[PhaseName]string{
		"throughput-mif:600":           "throughput-mif-600",
		"throughput-mif:400-label:hot": "throughput-mif-400-label-hot",
		"fill-pct:80":                  "fill-pct-80",
		"throughput-mif600":            "throughput-mif600", // legacy names pass through
		"probe":                        "probe",
	}
	for phase, want := range tests {
		if got := fileSafePhase(phase); got != want {
			t.Errorf("fileSafePhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

// mustParsePhases parses a phase list that the test requires to be valid.
func mustParsePhases(t *testing.T, cfg Config, raws ...string) []Phase {
	t.Helper()
	phases, err := parsePhases(raws, cfg)
	if err != nil {
		t.Fatalf("parsePhases(%v): %v", raws, err)
	}
	return phases
}

func TestResolveFillSizes(t *testing.T) {
	// 20 nodes * 110 slots, ~100 system pods already running.
	info := &ClusterInfo{Nodes: 20, PodCapacity: 2200, PreexistingPods: 100}
	cfg := Config{
		FillPerNode:       10,
		CreateConcurrency: 50,
		ThroughputCount:   500,
	}
	phases := mustParsePhases(t, cfg,
		"fill",               // 10 * 20 = 200
		"throughput-mif:600", // not a fill: leaves nothing resident
		"fill-pct:80",        // 80% of 2200 = 1760, minus 100 resident + 200 fill = 1460
		"fill-pct:50",        // target 1100 < 1760 already resident: 0
		"fill-label:again",   // plain fill is unconditional: another 200
	)
	if err := resolvePhases(phases, cfg, info); err != nil {
		t.Fatalf("resolvePhases: %v", err)
	}
	want := []int{200, 500, 1460, 0, 200} // non-fill entries report their own Requested
	for i, p := range phases {
		if got := p.Requested(); got != want[i] {
			t.Errorf("phase %s Requested() = %d, want %d", p.Name(), got, want[i])
		}
	}
}

func TestResolvePhasesCapacityOrdering(t *testing.T) {
	info := &ClusterInfo{Nodes: 20, PodCapacity: 2200, PreexistingPods: 100}
	cfg := Config{
		FillPerNode:       10,
		CreateConcurrency: 50,
		ThroughputCount:   500,
	}

	// mif600 BEFORE fill-pct:80: peak is the 1760 residents during the
	// fill, which fits.
	before := mustParsePhases(t, cfg, "fill", "throughput-mif:600", "fill-pct:80", "throughput-mif:400-label:hot")
	if err := resolvePhases(before, cfg, info); err != nil {
		t.Errorf("resolvePhases(mif600 before fill-pct:80) = %v, want nil", err)
	}

	// The same mif600 AFTER fill-pct:80 rides on 1760 residents: 2360 > 2200.
	after := mustParsePhases(t, cfg, "fill", "fill-pct:80", "throughput-mif:600")
	err := resolvePhases(after, cfg, info)
	if err == nil {
		t.Fatal("resolvePhases(mif600 after fill-pct:80) = nil, want error")
	}
	if !strings.Contains(err.Error(), "throughput-mif:600") {
		t.Errorf("capacity error should name the peak phase, got: %v", err)
	}
}
