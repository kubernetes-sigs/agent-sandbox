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
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func fixedRng() *rand.Rand {
	return rand.New(rand.NewPCG(0xA5A5A5A5, 0x5A5A5A5A))
}

func TestPoissonScheduleStrictlyIncreasing(t *testing.T) {
	sched := newPoissonSchedule(1000, fixedRng())
	prev := time.Duration(-1)
	for i := range 10000 {
		next := sched.Next()
		if next <= prev {
			t.Fatalf("arrival %d: offset %v not strictly greater than previous %v", i, next, prev)
		}
		prev = next
	}
}

// TestPoissonScheduleRate verifies the process realizes the target rate: the
// mean inter-arrival gap approximates 1/rate, and the number of arrivals in
// a window of T seconds approximates rate*T.
func TestPoissonScheduleRate(t *testing.T) {
	for _, rate := range []float64{5, 300, 2000} {
		sched := newPoissonSchedule(rate, fixedRng())
		const n = 200000
		var last time.Duration
		for range n {
			last = sched.Next()
		}
		gotMean := last.Seconds() / n
		wantMean := 1 / rate
		if math.Abs(gotMean-wantMean)/wantMean > 0.02 {
			t.Errorf("rate %.0f: mean inter-arrival %.6fs, want %.6fs (+/-2%%)", rate, gotMean, wantMean)
		}
	}

	// Arrival count within a fixed window, as the phase's pacing loop uses it.
	const rate, seconds = 300.0, 60.0
	sched := newPoissonSchedule(rate, fixedRng())
	duration := time.Duration(seconds * float64(time.Second))
	count := 0
	for sched.Next() <= duration {
		count++
	}
	expected := rate * seconds
	if math.Abs(float64(count)-expected)/expected > 0.05 {
		t.Errorf("arrivals in %vs = %d, want ~%.0f (+/-5%%)", seconds, count, expected)
	}
}

// TestPoissonScheduleIsExponential checks the jitter really is Poisson and
// not, say, uniform: exponential inter-arrival gaps have a coefficient of
// variation of 1 (uniform jitter would give ~0.58).
func TestPoissonScheduleIsExponential(t *testing.T) {
	const rate = 100.0
	const n = 100000
	sched := newPoissonSchedule(rate, fixedRng())
	gaps := make([]float64, n)
	prev := time.Duration(0)
	var sum float64
	for i := range gaps {
		next := sched.Next()
		gaps[i] = (next - prev).Seconds()
		sum += gaps[i]
		prev = next
	}
	mean := sum / n
	var variance float64
	for _, g := range gaps {
		variance += (g - mean) * (g - mean)
	}
	variance /= n
	cv := math.Sqrt(variance) / mean
	if math.Abs(cv-1) > 0.05 {
		t.Errorf("coefficient of variation = %.3f, want ~1.0 (exponential inter-arrivals)", cv)
	}
}

func TestSustainedPoolReplicasPerNamespace(t *testing.T) {
	tests := []struct {
		name       string
		rate       float64
		namespaces int
		headroom   time.Duration
		want       int
	}{
		{"default sizing", 300, 1, 10 * time.Second, 3000},
		{"split across namespaces", 300, 4, 10 * time.Second, 750},
		{"rounds up", 100, 3, 10 * time.Second, 334}, // 100/3*10 = 333.33...
		{"small rate", 0.5, 1, 10 * time.Second, 5},
		{"sub-replica rounds to one", 0.05, 1, 10 * time.Second, 1},
		{"zero rate", 0, 1, 10 * time.Second, 0},
		{"invalid namespaces", 300, 0, 10 * time.Second, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				SustainedRate:         tc.rate,
				SustainedNamespaces:   tc.namespaces,
				SustainedPoolHeadroom: tc.headroom,
			}
			if got := sustainedPoolReplicasPerNamespace(cfg); got != tc.want {
				t.Errorf("sustainedPoolReplicasPerNamespace(rate=%v ns=%d headroom=%v) = %d, want %d",
					tc.rate, tc.namespaces, tc.headroom, got, tc.want)
			}
		})
	}
}

func TestSustainedExpectedClaims(t *testing.T) {
	cfg := Config{SustainedRate: 300, SustainedSeconds: 60}
	if got := sustainedExpectedClaims(cfg); got != 18000 {
		t.Errorf("sustainedExpectedClaims = %d, want 18000", got)
	}
}

func TestCheckClusterCapacitySustained(t *testing.T) {
	base := Config{
		Phases:                   []string{string(PhaseClaimsWarmSustained)},
		SustainedRate:            100,
		SustainedNamespaces:      1,
		SustainedPoolHeadroom:    10 * time.Second,
		ClaimDwell:               5 * time.Second,
		SustainedLifecycleBudget: 5 * time.Second,
	}
	// pool = ceil(100*10) = 1000, inFlight = ceil(100*(5+5)) = 1000 => 2000.
	if err := checkClusterCapacity(base, &ClusterInfo{PodCapacity: 2000}); err != nil {
		t.Errorf("capacity 2000 should fit needed 2000: %v", err)
	}
	if err := checkClusterCapacity(base, &ClusterInfo{PodCapacity: 1999}); err == nil {
		t.Error("capacity 1999 should fail for needed 2000")
	}

	// A slower assumed ready+delete pipeline must raise the requirement:
	// budget 15s => inFlight = ceil(100*(5+15)) = 2000 => needed 3000.
	slow := base
	slow.SustainedLifecycleBudget = 15 * time.Second
	if err := checkClusterCapacity(slow, &ClusterInfo{PodCapacity: 2000}); err == nil {
		t.Error("capacity 2000 should fail once the lifecycle budget raises needed to 3000")
	}
	if err := checkClusterCapacity(slow, &ClusterInfo{PodCapacity: 3000}); err != nil {
		t.Errorf("capacity 3000 should fit needed 3000: %v", err)
	}
}
