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
	"testing"
	"time"
)

// rec builds a SandboxRecord with CreateCalled/SandboxReady at the given
// offsets from base (negative offset = field left zero).
func rec(base time.Time, createOffset, readyOffset time.Duration) SandboxRecord {
	r := SandboxRecord{}
	if createOffset >= 0 {
		r.CreateCalled = base.Add(createOffset)
	}
	if readyOffset >= 0 {
		r.SandboxReady = base.Add(readyOffset)
	}
	return r
}

func TestComputeTimeToAllReady(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		records []SandboxRecord
		want    *float64 // nil means "expect nil"
	}{
		{
			name:    "no records",
			records: nil,
			want:    nil,
		},
		{
			name: "single record",
			records: []SandboxRecord{
				rec(base, 0, 1500*time.Millisecond),
			},
			want: new(1.5),
		},
		{
			name: "spans first create to last ready",
			records: []SandboxRecord{
				rec(base, 100*time.Millisecond, 2*time.Second),
				rec(base, 0, 5*time.Second), // earliest create, latest ready
				rec(base, 200*time.Millisecond, 3*time.Second),
			},
			want: new(5.0),
		},
		{
			name: "last ready not on last created",
			records: []SandboxRecord{
				rec(base, 0, 10*time.Second),
				rec(base, time.Second, 2*time.Second),
			},
			want: new(10.0),
		},
		{
			name: "one record never ready",
			records: []SandboxRecord{
				rec(base, 0, 2*time.Second),
				rec(base, 0, -1), // ready never observed
			},
			want: nil,
		},
		{
			name: "record without create call",
			records: []SandboxRecord{
				rec(base, -1, 2*time.Second),
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeTimeToAllReady(tc.records)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("computeTimeToAllReady() = %v, want %v", fmtPtr(got), fmtPtr(tc.want))
			}
			if got != nil && math.Abs(*got-*tc.want) > 1e-9 {
				t.Errorf("computeTimeToAllReady() = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestComputeLatencyBreakdownClaimRecords(t *testing.T) {
	// Claim records only have CreateCalled/CreateReturned/SandboxReady set
	// (no pod milestones); the breakdown must expose CreateAck and
	// EndToEndReady and leave the pod-stage intervals nil.
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	records := []SandboxRecord{
		{
			CreateCalled:   base,
			CreateReturned: base.Add(50 * time.Millisecond),
			SandboxReady:   base.Add(2 * time.Second),
		},
		{
			CreateCalled:   base.Add(10 * time.Millisecond),
			CreateReturned: base.Add(40 * time.Millisecond),
			SandboxReady:   base.Add(4*time.Second + 10*time.Millisecond),
		},
	}

	b := computeLatencyBreakdown(records)

	if b.CreateAck == nil || b.CreateAck.Count != 2 {
		t.Fatalf("CreateAck = %+v, want count 2", b.CreateAck)
	}
	if got, want := b.CreateAck.MaxMs, 50.0; math.Abs(got-want) > 1e-6 {
		t.Errorf("CreateAck.MaxMs = %v, want %v", got, want)
	}
	if b.EndToEndReady == nil || b.EndToEndReady.Count != 2 {
		t.Fatalf("EndToEndReady = %+v, want count 2", b.EndToEndReady)
	}
	if got, want := b.EndToEndReady.MaxMs, 4000.0; math.Abs(got-want) > 1e-6 {
		t.Errorf("EndToEndReady.MaxMs = %v, want %v", got, want)
	}
	for name, stats := range map[string]*LatencyStats{
		"CreateToPodCreated":     b.CreateToPodCreated,
		"PodCreatedToScheduled":  b.PodCreatedToScheduled,
		"ScheduledToPodRunning":  b.ScheduledToPodRunning,
		"PodRunningToPodReady":   b.PodRunningToPodReady,
		"PodReadyToSandboxReady": b.PodReadyToSandboxReady,
	} {
		if stats != nil {
			t.Errorf("%s = %+v, want nil (claim records have no pod milestones)", name, stats)
		}
	}
}

func TestComputeLatencyStatsPercentiles(t *testing.T) {
	// 1..100 ms: nearest-rank percentiles are exact.
	var durations []time.Duration
	for i := 1; i <= 100; i++ {
		durations = append(durations, time.Duration(i)*time.Millisecond)
	}

	stats := computeLatencyStats(durations)
	if stats == nil {
		t.Fatal("computeLatencyStats() = nil, want stats")
	}
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"Count", float64(stats.Count), 100},
		{"MinMs", stats.MinMs, 1},
		{"MeanMs", stats.MeanMs, 50.5},
		{"P50Ms", stats.P50Ms, 50},
		{"P90Ms", stats.P90Ms, 90},
		{"P99Ms", stats.P99Ms, 99},
		{"MaxMs", stats.MaxMs, 100},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > 1e-6 {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if got := computeLatencyStats(nil); got != nil {
		t.Errorf("computeLatencyStats(nil) = %+v, want nil", got)
	}
}

func fmtPtr(f *float64) any {
	if f == nil {
		return "<nil>"
	}
	return *f
}
