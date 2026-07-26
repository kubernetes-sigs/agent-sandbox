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
	"slices"
	"time"
)

// LatencyStats holds summary statistics for a set of measured durations.
type LatencyStats struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"minMs"`
	MeanMs float64 `json:"meanMs"`
	P50Ms  float64 `json:"p50Ms"`
	P90Ms  float64 `json:"p90Ms"`
	P95Ms  float64 `json:"p95Ms"`
	P99Ms  float64 `json:"p99Ms"`
	MaxMs  float64 `json:"maxMs"`
}

// computeLatencyStats summarizes the given durations; returns nil if there are none.
func computeLatencyStats(durations []time.Duration) *LatencyStats {
	if len(durations) == 0 {
		return nil
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	return &LatencyStats{
		Count:  len(sorted),
		MinMs:  toMs(sorted[0]),
		MeanMs: toMs(sum / time.Duration(len(sorted))),
		P50Ms:  toMs(percentileSorted(sorted, 50)),
		P90Ms:  toMs(percentileSorted(sorted, 90)),
		P95Ms:  toMs(percentileSorted(sorted, 95)),
		P99Ms:  toMs(percentileSorted(sorted, 99)),
		MaxMs:  toMs(sorted[len(sorted)-1]),
	}
}

// percentileSorted returns the nearest-rank percentile of an already-sorted slice.
func percentileSorted(sorted []time.Duration, pct float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := min(max(int(math.Ceil(pct/100.0*float64(len(sorted)))), 1), len(sorted))
	return sorted[rank-1] // nearest-rank, 1-indexed
}

func toMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// LatencyBreakdown decomposes end-to-end sandbox launch latency into stages,
// each computed from client-observed watch timestamps.
type LatencyBreakdown struct {
	// CreateAck: Create call issued -> Create call returned (apiserver write path).
	CreateAck *LatencyStats `json:"createAck,omitempty"`
	// CreateToPodCreated: Create call issued -> backing Pod first observed
	// (sandbox controller reconcile + Pod write).
	CreateToPodCreated *LatencyStats `json:"createToPodCreated,omitempty"`
	// PodCreatedToScheduled: Pod first observed -> PodScheduled=True (scheduler).
	PodCreatedToScheduled *LatencyStats `json:"podCreatedToScheduled,omitempty"`
	// ScheduledToPodRunning: PodScheduled=True -> phase=Running (kubelet, image pull, container start).
	ScheduledToPodRunning *LatencyStats `json:"scheduledToPodRunning,omitempty"`
	// PodRunningToPodReady: phase=Running -> Pod Ready=True (readiness propagation).
	PodRunningToPodReady *LatencyStats `json:"podRunningToPodReady,omitempty"`
	// PodReadyToSandboxReady: Pod Ready=True -> Sandbox Ready=True (sandbox controller status propagation).
	PodReadyToSandboxReady *LatencyStats `json:"podReadyToSandboxReady,omitempty"`
	// EndToEndReady: Create call issued -> Sandbox Ready=True.
	EndToEndReady *LatencyStats `json:"endToEndReady,omitempty"`
}

func computeLatencyBreakdown(records []SandboxRecord) LatencyBreakdown {
	interval := func(from, to func(*SandboxRecord) time.Time) *LatencyStats {
		var durations []time.Duration
		for i := range records {
			rec := &records[i]
			start, end := from(rec), to(rec)
			if start.IsZero() || end.IsZero() {
				continue
			}
			durations = append(durations, end.Sub(start))
		}
		return computeLatencyStats(durations)
	}

	createCalled := func(r *SandboxRecord) time.Time { return r.CreateCalled }
	createReturned := func(r *SandboxRecord) time.Time { return r.CreateReturned }
	podCreated := func(r *SandboxRecord) time.Time { return r.PodCreated }
	podScheduled := func(r *SandboxRecord) time.Time { return r.PodScheduled }
	podRunning := func(r *SandboxRecord) time.Time { return r.PodRunning }
	podReady := func(r *SandboxRecord) time.Time { return r.PodReady }
	sandboxReady := func(r *SandboxRecord) time.Time { return r.SandboxReady }

	return LatencyBreakdown{
		CreateAck:              interval(createCalled, createReturned),
		CreateToPodCreated:     interval(createCalled, podCreated),
		PodCreatedToScheduled:  interval(podCreated, podScheduled),
		ScheduledToPodRunning:  interval(podScheduled, podRunning),
		PodRunningToPodReady:   interval(podRunning, podReady),
		PodReadyToSandboxReady: interval(podReady, sandboxReady),
		EndToEndReady:          interval(createCalled, sandboxReady),
	}
}

// computeTimeToAllReady returns the seconds from the first Create call to the
// last observed Ready across the given records: how long until every request
// in the batch was Ready. It returns nil when there are no records or when
// any record failed to reach Ready (the batch never became "all ready").
//
// This is the headline metric for burst phases like claims-warm, where all
// creates are issued at once and the question is when the last claim became
// Ready, not just the per-claim percentiles.
func computeTimeToAllReady(records []SandboxRecord) *float64 {
	if len(records) == 0 {
		return nil
	}
	var firstCreate, lastReady time.Time
	for i := range records {
		rec := &records[i]
		if rec.CreateCalled.IsZero() || rec.SandboxReady.IsZero() {
			return nil
		}
		if firstCreate.IsZero() || rec.CreateCalled.Before(firstCreate) {
			firstCreate = rec.CreateCalled
		}
		if rec.SandboxReady.After(lastReady) {
			lastReady = rec.SandboxReady
		}
	}
	seconds := lastReady.Sub(firstCreate).Seconds()
	return &seconds
}

// WindowedLatency aggregates create->Ready latency over one fixed window of
// ARRIVALS: records are bucketed by CreateCalled, so each window reflects the
// experience of the claims that arrived during it. In a sustained-rate phase,
// latency degradation over time shows up as later windows getting slower.
type WindowedLatency struct {
	// StartOffsetSeconds/EndOffsetSeconds bound the window [start, end)
	// relative to the earliest CreateCalled among the records.
	StartOffsetSeconds float64 `json:"startOffsetSeconds"`
	EndOffsetSeconds   float64 `json:"endOffsetSeconds"`
	// Arrivals counts records whose CreateCalled fell in this window;
	// Ready counts how many of those were observed Ready (ever, not
	// necessarily within the window).
	Arrivals int `json:"arrivals"`
	Ready    int `json:"ready"`
	// Latency summarizes create->Ready for the Ready arrivals; nil when none.
	Latency *LatencyStats `json:"latency,omitempty"`
}

// computeWindowedLatencies buckets records into contiguous windows of the
// given size by CreateCalled and summarizes create->Ready latency per window.
// Records without a CreateCalled are skipped; empty interior windows are
// retained (Arrivals=0) so the timeline stays contiguous.
func computeWindowedLatencies(records []SandboxRecord, window time.Duration) []WindowedLatency {
	if window <= 0 {
		return nil
	}
	var base, last time.Time
	for i := range records {
		t := records[i].CreateCalled
		if t.IsZero() {
			continue
		}
		if base.IsZero() || t.Before(base) {
			base = t
		}
		if t.After(last) {
			last = t
		}
	}
	if base.IsZero() {
		return nil
	}

	numWindows := int(last.Sub(base)/window) + 1
	out := make([]WindowedLatency, numWindows)
	durations := make([][]time.Duration, numWindows)
	for i := range out {
		out[i].StartOffsetSeconds = (time.Duration(i) * window).Seconds()
		out[i].EndOffsetSeconds = (time.Duration(i+1) * window).Seconds()
	}
	for i := range records {
		rec := &records[i]
		if rec.CreateCalled.IsZero() {
			continue
		}
		idx := int(rec.CreateCalled.Sub(base) / window)
		out[idx].Arrivals++
		if rec.SandboxReady.IsZero() {
			continue
		}
		out[idx].Ready++
		durations[idx] = append(durations[idx], rec.SandboxReady.Sub(rec.CreateCalled))
	}
	for i := range out {
		out[i].Latency = computeLatencyStats(durations[i])
	}
	return out
}

// ThroughputStats summarizes the rate at which a set of events occurred.
type ThroughputStats struct {
	Count           int     `json:"count"`
	DurationSeconds float64 `json:"durationSeconds"`
	// OverallPerSecond is count / (last - first).
	OverallPerSecond float64 `json:"overallPerSecond"`
	// SteadyStatePerSecond excludes the first and last 10% of events,
	// removing ramp-up and drain effects.
	SteadyStatePerSecond float64 `json:"steadyStatePerSecond"`
	// BestWindow rates report the highest rate seen in any sliding window.
	Best10sPerSecond float64 `json:"best10sPerSecond"`
	Best60sPerSecond float64 `json:"best60sPerSecond"`
}

// PerNodeRates is a ThroughputStats scaled to a single worker node.
// Node-side pod startup is typically the bottleneck, so aggregate throughput
// scales with worker count; per-node rates are comparable across cluster sizes.
type PerNodeRates struct {
	WorkerNodes          int     `json:"workerNodes"`
	OverallPerSecond     float64 `json:"overallPerSecond"`
	SteadyStatePerSecond float64 `json:"steadyStatePerSecond"`
	Best10sPerSecond     float64 `json:"best10sPerSecond"`
	Best60sPerSecond     float64 `json:"best60sPerSecond"`
}

// perNode divides the rates across workerNodes; returns nil if either is unknown.
func (t *ThroughputStats) perNode(workerNodes int) *PerNodeRates {
	if t == nil || workerNodes <= 0 {
		return nil
	}
	return &PerNodeRates{
		WorkerNodes:          workerNodes,
		OverallPerSecond:     t.OverallPerSecond / float64(workerNodes),
		SteadyStatePerSecond: t.SteadyStatePerSecond / float64(workerNodes),
		Best10sPerSecond:     t.Best10sPerSecond / float64(workerNodes),
		Best60sPerSecond:     t.Best60sPerSecond / float64(workerNodes),
	}
}

// computeThroughputStats summarizes the rate of the given event times; returns nil if there are fewer than 2.
func computeThroughputStats(times []time.Time) *ThroughputStats {
	if len(times) < 2 {
		return nil
	}
	sorted := slices.Clone(times)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })

	n := len(sorted)
	duration := sorted[n-1].Sub(sorted[0])

	stats := &ThroughputStats{
		Count:           n,
		DurationSeconds: duration.Seconds(),
	}
	if duration > 0 {
		stats.OverallPerSecond = float64(n) / duration.Seconds()
	}

	// Steady state: middle 80% of events by rank.
	lo, hi := n/10, n-1-n/10
	if hi > lo {
		steadyDuration := sorted[hi].Sub(sorted[lo])
		if steadyDuration > 0 {
			stats.SteadyStatePerSecond = float64(hi-lo+1) / steadyDuration.Seconds()
		}
	}

	stats.Best10sPerSecond = bestWindowRate(sorted, 10*time.Second)
	stats.Best60sPerSecond = bestWindowRate(sorted, 60*time.Second)
	return stats
}

// bestWindowRate returns the highest events-per-second seen in any sliding window of the given size.
func bestWindowRate(sorted []time.Time, window time.Duration) float64 {
	best := 0
	i := 0
	for j := range sorted {
		for sorted[j].Sub(sorted[i]) > window {
			i++
		}
		if count := j - i + 1; count > best {
			best = count
		}
	}
	return float64(best) / window.Seconds()
}

// TimeseriesPoint is one line of timeseries.jsonl: per-second event counts
// plus gauges, useful for plotting throughput and detecting stalls.
type TimeseriesPoint struct {
	Time          time.Time `json:"time"`
	OffsetSeconds int       `json:"offsetSeconds"`

	CreateCalled int `json:"createCalled,omitempty"`
	PodCreated   int `json:"podCreated,omitempty"`
	PodScheduled int `json:"podScheduled,omitempty"`
	PodRunning   int `json:"podRunning,omitempty"`
	SandboxReady int `json:"sandboxReady,omitempty"`
	PodDeleted   int `json:"podDeleted,omitempty"`

	// LivePods is the number of test Pods that exist at the end of this second
	// (observed created and not yet observed deleted).
	LivePods int `json:"livePods"`
	// CumulativeReady is the total number of sandboxes observed Ready so far.
	CumulativeReady int `json:"cumulativeReady"`
}

// buildTimeseries aggregates per-sandbox milestones into per-second buckets.
func buildTimeseries(records []SandboxRecord) []TimeseriesPoint {
	var base, last time.Time
	for i := range records {
		for _, ts := range []time.Time{records[i].CreateCalled, records[i].SandboxReady, records[i].PodDeleted} {
			if ts.IsZero() {
				continue
			}
			if base.IsZero() || ts.Before(base) {
				base = ts
			}
			if ts.After(last) {
				last = ts
			}
		}
	}
	if base.IsZero() {
		return nil
	}

	base = base.Truncate(time.Second)
	numBuckets := int(last.Sub(base)/time.Second) + 1
	points := make([]TimeseriesPoint, numBuckets)
	for i := range points {
		points[i].Time = base.Add(time.Duration(i) * time.Second)
		points[i].OffsetSeconds = i
	}

	bucket := func(ts time.Time) int {
		if ts.IsZero() {
			return -1
		}
		idx := int(ts.Sub(base) / time.Second)
		if idx < 0 || idx >= numBuckets {
			return -1
		}
		return idx
	}

	for i := range records {
		rec := &records[i]
		if idx := bucket(rec.CreateCalled); idx >= 0 {
			points[idx].CreateCalled++
		}
		if idx := bucket(rec.PodCreated); idx >= 0 {
			points[idx].PodCreated++
		}
		if idx := bucket(rec.PodScheduled); idx >= 0 {
			points[idx].PodScheduled++
		}
		if idx := bucket(rec.PodRunning); idx >= 0 {
			points[idx].PodRunning++
		}
		if idx := bucket(rec.SandboxReady); idx >= 0 {
			points[idx].SandboxReady++
		}
		if idx := bucket(rec.PodDeleted); idx >= 0 {
			points[idx].PodDeleted++
		}
	}

	livePods := 0
	cumulativeReady := 0
	for i := range points {
		livePods += points[i].PodCreated - points[i].PodDeleted
		cumulativeReady += points[i].SandboxReady
		points[i].LivePods = livePods
		points[i].CumulativeReady = cumulativeReady
	}
	return points
}
