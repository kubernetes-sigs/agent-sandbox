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
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
)

// Phase is one parsed entry of --phases: a phase kind plus its arguments.
//
// Lifecycle: parsePhase turns the raw string into a Phase at flag time,
// Validate rejects bad flag combinations before any cluster interaction,
// Resolve sizes the phase once the cluster has been inspected (fills depend
// on node count / pod capacity and on the pods left behind by earlier
// phases), and Run executes it.
type Phase interface {
	// Name is the display name recorded in logs, summary.json and the
	// report. It is the raw --phases entry, so a run that repeats a kind
	// keeps distinct names via label arguments (throughput-mif:400-label:x).
	Name() PhaseName

	// Kind is the phase's base kind (fill, probe, throughput, claims-warm,
	// claims-warm-sustained), unaffected by arguments: a labeled entry like
	// probe-label:x still reports as kind probe, which is what selects the
	// kind-specific output in summary.json and printReport.
	Kind() PhaseName

	// Validate rejects configurations that can be caught from flags alone.
	Validate(cfg Config) error

	// Resolve sizes the phase against the inspected cluster. resident is
	// the worker-pod count when the phase starts (pre-existing pods plus
	// earlier fills); it returns the resident count when the phase ends
	// and the peak concurrent pods while it runs. resolvePhases walks the
	// phase list calling this in order, and feeds peak into the capacity
	// preflight.
	Resolve(cfg Config, info *ClusterInfo, resident int) (after, peak int)

	// Description is a one-line explanation of the resolved sizing,
	// logged before the phases run. Call Resolve first.
	Description() string

	// Requested is the number of creations the phase intends to make.
	// Call Resolve first.
	Requested() int

	// Run executes the phase.
	Run(ctx context.Context, test *stressTest, number PhaseNumber) error
}

// parsePhase parses one --phases entry. The grammar is a phase kind,
// optionally followed by hyphen-separated key:value arguments:
//
//	fill                          fill-per-node * worker-nodes background sandboxes
//	fill-pct:80                   top the cluster up to 80% total pod utilization
//	probe                         low-concurrency launch-latency probes
//	throughput-mif:400            closed-loop churn, 400 in flight
//	throughput-mif:400-label:hot  same, with a distinct name for reports
//	claims-warm                   SandboxClaim burst against a warm pool
//	claims-warm-sustained         Poisson claim stream at a target rate
//
// Every kind accepts label:<x> so repeated entries keep distinct names;
// labels are limited to [a-z0-9_.] to keep the grammar unambiguous.
// throughput-mifN and bare throughput (mif 50) are accepted as the legacy
// spellings.
func parsePhase(raw string) (Phase, error) {
	kind, rest := raw, ""
	// Kind names themselves contain hyphens (claims-warm-sustained), so
	// match the longest known kind first, then treat any remainder as
	// arguments.
	found := false
	for _, k := range []PhaseName{PhaseClaimsWarmSustained, PhaseClaimsWarm, PhaseThroughput, PhaseProbe, PhaseFill} {
		if raw == string(k) {
			kind, found = string(k), true
			break
		}
		if s, ok := strings.CutPrefix(raw, string(k)+"-"); ok {
			kind, rest, found = string(k), s, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown phase %q (want fill, fill-pct:N, probe, claims-warm, claims-warm-sustained, or throughput-mif:N)", raw)
	}

	// Legacy spellings: bare "throughput" is mif 50 (renamed so the report
	// shows the level), and "throughput-mifN" predates the key:value form.
	if kind == string(PhaseThroughput) {
		if rest == "" {
			return &throughputPhase{raw: PhaseName(raw) + "-mif50", mif: 50}, nil
		}
		if s, ok := strings.CutPrefix(rest, "mif"); ok && !strings.Contains(rest, ":") {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("phase %q: mif must be a positive integer", raw)
			}
			return &throughputPhase{raw: PhaseName(raw), mif: n}, nil
		}
	}

	args, err := parsePhaseArgs(rest)
	if err != nil {
		return nil, fmt.Errorf("phase %q: %w", raw, err)
	}
	take := func(key string) (string, bool) {
		v, ok := args[key]
		delete(args, key)
		return v, ok
	}
	if label, ok := take("label"); ok {
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' {
				return nil, fmt.Errorf("phase %q: label may only contain [a-z0-9_.]", raw)
			}
		}
	}

	var phase Phase
	switch kind {
	case string(PhaseFill):
		p := &fillPhase{raw: PhaseName(raw)}
		if v, ok := take("pct"); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return nil, fmt.Errorf("phase %q: pct must be an integer in 1..100", raw)
			}
			p.pct = n
		}
		phase = p
	case string(PhaseThroughput):
		v, ok := take("mif")
		if !ok {
			return nil, fmt.Errorf("phase %q: throughput requires mif:N", raw)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("phase %q: mif must be a positive integer", raw)
		}
		phase = &throughputPhase{raw: PhaseName(raw), mif: n}
	case string(PhaseProbe):
		phase = &probePhase{raw: PhaseName(raw)}
	case string(PhaseClaimsWarm):
		phase = &claimsWarmPhase{raw: PhaseName(raw)}
	case string(PhaseClaimsWarmSustained):
		phase = &claimsWarmSustainedPhase{raw: PhaseName(raw)}
	}
	for key := range args {
		return nil, fmt.Errorf("phase %q: unknown argument %q", raw, key)
	}
	return phase, nil
}

// parsePhaseArgs splits "pct:80-label:foo" into {"pct": "80", "label": "foo"}.
func parsePhaseArgs(rest string) (map[string]string, error) {
	args := map[string]string{}
	if rest == "" {
		return args, nil
	}
	for seg := range strings.SplitSeq(rest, "-") {
		key, value, ok := strings.Cut(seg, ":")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("argument %q is not key:value", seg)
		}
		if _, dup := args[key]; dup {
			return nil, fmt.Errorf("duplicate argument %q", key)
		}
		args[key] = value
	}
	return args, nil
}

// parsePhases parses and validates the full --phases list.
func parsePhases(raws []string, cfg Config) ([]Phase, error) {
	phases := make([]Phase, 0, len(raws))
	for _, raw := range raws {
		p, err := parsePhase(raw)
		if err != nil {
			return nil, err
		}
		if err := p.Validate(cfg); err != nil {
			return nil, err
		}
		phases = append(phases, p)
	}
	return phases, nil
}

// resolvePhases sizes each phase against the inspected cluster and returns
// an error when the configuration would exceed spare pod capacity: in that
// case latency and throughput results would measure queueing for capacity
// rather than the sandbox launch pipeline, so the run would not test what
// it claims to test.
//
// Phases run sequentially and fill sandboxes stay up, so the walk tracks
// the resident pod count in phase order — a throughput-mif:600 fits before
// a fill-pct:80 but not after it — and the error names the phase where the
// peak lands.
func resolvePhases(phases []Phase, cfg Config, info *ClusterInfo) error {
	resident := info.PreexistingPods
	peak, peakPhase := 0, PhaseName("")
	claimsWarm := false
	for _, p := range phases {
		var phasePeak int
		resident, phasePeak = p.Resolve(cfg, info, resident)
		if phasePeak > peak {
			peak, peakPhase = phasePeak, p.Name()
		}
		if _, ok := p.(*claimsWarmPhase); ok {
			claimsWarm = true
		}
	}
	needed := peak - info.PreexistingPods
	spare := info.PodCapacity - info.PreexistingPods
	if needed <= 0 {
		return nil
	}
	switch {
	case needed > spare:
		return fmt.Errorf("test needs up to %d concurrent pods (peak during phase %q) but the cluster only has %d spare pod slots; results would measure capacity queueing, not launch performance. Reduce --fill-per-node / fill-pct / --claims-warm-count / throughput-mif or add nodes", needed, peakPhase, spare)
	case spare > 0 && needed > spare*9/10:
		log.Printf("WARNING: test needs up to %d concurrent pods (peak during phase %q), over 90%% of the %d spare pod slots; scheduling may interfere with measurements.", needed, peakPhase, spare)
	}
	// The warm pool controller replenishes claimed sandboxes, so claims-warm
	// can transiently reach ~2x its count. Warn rather than fail:
	// replenishment pods only queue (they do not delay the claims being
	// measured) and cleanup removes them.
	if claimsWarm && 2*cfg.ClaimsWarmCount > spare {
		log.Printf("WARNING: claims-warm pool replenishment can transiently need up to %d pods (2x --claims-warm-count) but only %d spare pod slots exist; replenishment pods will queue on capacity. Add nodes or reduce --claims-warm-count.", 2*cfg.ClaimsWarmCount, spare)
	}
	return nil
}

// hasClaimsPhase reports whether any phase needs the SandboxClaim machinery
// (claim watch, extensions CRDs, controller profiler). The claim watch is
// cluster-wide, so it also covers the sustained phase's extra <ns>-s1..sN
// shard namespaces.
func hasClaimsPhase(phases []Phase) bool {
	for _, p := range phases {
		switch p.(type) {
		case *claimsWarmPhase, *claimsWarmSustainedPhase:
			return true
		}
	}
	return false
}

// fillPhase implements fill and fill-pct:N: long-running background
// sandboxes that stay up for the rest of the run, so later phases measure a
// cluster at scale. pct == 0 sizes by --fill-per-node * worker nodes;
// otherwise the phase tops the cluster up to pct% of total worker pod
// capacity, counting the pods already there (pre-existing system pods and
// earlier fills).
type fillPhase struct {
	raw PhaseName
	pct int // 0 = FillPerNode * worker nodes

	// Set by Resolve.
	count    int
	perNode  int
	nodes    int
	capacity int
}

func (p *fillPhase) Name() PhaseName { return p.raw }

func (p *fillPhase) Kind() PhaseName { return PhaseFill }

func (p *fillPhase) Validate(cfg Config) error {
	if p.pct == 0 && cfg.FillPerNode <= 0 {
		return fmt.Errorf("phase %q requires --fill-per-node > 0", p.raw)
	}
	if cfg.CreateConcurrency <= 0 {
		return fmt.Errorf("--create-concurrency must be > 0 for phase %q", p.raw)
	}
	return nil
}

func (p *fillPhase) Resolve(cfg Config, info *ClusterInfo, resident int) (int, int) {
	p.perNode, p.nodes, p.capacity = cfg.FillPerNode, info.Nodes, info.PodCapacity
	if p.pct == 0 {
		p.count = cfg.FillPerNode * info.Nodes
	} else {
		p.count = max(0, info.PodCapacity*p.pct/100-resident)
	}
	return resident + p.count, resident + p.count
}

func (p *fillPhase) Description() string {
	if p.pct == 0 {
		return fmt.Sprintf("%s: %d sandboxes (%d per worker node * %d nodes)", p.raw, p.count, p.perNode, p.nodes)
	}
	return fmt.Sprintf("%s: %d sandboxes (top up to %d%% of %d worker pod slots)", p.raw, p.count, p.pct, p.capacity)
}

func (p *fillPhase) Requested() int { return p.count }

func (p *fillPhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runFillPhase(ctx, p.raw, number, p.count)
}

// probePhase measures clean launch latency at the current cluster scale.
type probePhase struct {
	raw PhaseName

	// Set by Resolve.
	count       int
	concurrency int
}

func (p *probePhase) Name() PhaseName { return p.raw }

func (p *probePhase) Kind() PhaseName { return PhaseProbe }

func (p *probePhase) Validate(cfg Config) error {
	if cfg.ProbeCount <= 0 {
		return fmt.Errorf("phase %q requires --probe-count > 0", p.raw)
	}
	if cfg.ProbeConcurrency <= 0 {
		return fmt.Errorf("--probe-concurrency must be > 0 for phase %q", p.raw)
	}
	return nil
}

func (p *probePhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	p.count, p.concurrency = cfg.ProbeCount, cfg.ProbeConcurrency
	// Probes are deleted as they are measured, so they do not accumulate.
	return resident, resident + cfg.ProbeConcurrency
}

func (p *probePhase) Description() string {
	return fmt.Sprintf("%s: %d sandboxes at concurrency %d", p.raw, p.count, p.concurrency)
}

func (p *probePhase) Requested() int { return p.count }

func (p *probePhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runProbePhase(ctx, number)
}

// throughputPhase churns sandboxes (create -> ready -> delete) in a closed
// loop capped at mif in flight, measuring sustained ready/sec.
type throughputPhase struct {
	raw PhaseName
	mif int

	// Set by Resolve.
	count int
}

func (p *throughputPhase) Name() PhaseName { return p.raw }

func (p *throughputPhase) Kind() PhaseName { return PhaseThroughput }

func (p *throughputPhase) Validate(cfg Config) error {
	if cfg.ThroughputCount <= 0 {
		return fmt.Errorf("phase %q requires --throughput-count > 0", p.raw)
	}
	if cfg.CreateConcurrency <= 0 {
		return fmt.Errorf("--create-concurrency must be > 0 for phase %q", p.raw)
	}
	return nil
}

func (p *throughputPhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	p.count = cfg.ThroughputCount
	return resident, resident + p.mif
}

func (p *throughputPhase) Description() string {
	return fmt.Sprintf("%s: closed-loop churn, %d in flight, >=%d creations", p.raw, p.mif, p.count)
}

func (p *throughputPhase) Requested() int { return p.count }

func (p *throughputPhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	if test.profiler != nil {
		// 5s in to skip the slot-fill burst; levels last >=45s
		// (ThroughputMinSeconds), so the 20s window lands inside.
		go test.profiler.CaptureCPUProfile(ctx, p.raw, 5*time.Second, 20*time.Second)
	}
	return test.runThroughputLevel(ctx, p.raw, number, p.mif)
}

// claimsWarmPhase fires SandboxClaims simultaneously against a fully
// provisioned SandboxWarmPool, measuring claim-create -> claim-Ready latency.
type claimsWarmPhase struct {
	raw PhaseName

	// Set by Resolve.
	count int
}

func (p *claimsWarmPhase) Name() PhaseName { return p.raw }

func (p *claimsWarmPhase) Kind() PhaseName { return PhaseClaimsWarm }

func (p *claimsWarmPhase) Validate(cfg Config) error {
	if cfg.ClaimsWarmCount <= 0 {
		return fmt.Errorf("phase %q requires --claims-warm-count > 0", p.raw)
	}
	return nil
}

func (p *claimsWarmPhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	p.count = cfg.ClaimsWarmCount
	// The pool and its claims are cleaned up at the end of the phase.
	return resident, resident + p.count
}

func (p *claimsWarmPhase) Description() string {
	return fmt.Sprintf("%s: %d simultaneous claims against a %d-replica warm pool", p.raw, p.count, p.count)
}

func (p *claimsWarmPhase) Requested() int { return p.count }

func (p *claimsWarmPhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runClaimsWarmPhase(ctx, number)
}

// claimsWarmSustainedPhase streams SandboxClaims at a target rate with
// Poisson jitter against continuously replenished warm pools (sustained.go).
type claimsWarmSustainedPhase struct {
	raw PhaseName

	// Set by Resolve.
	expected int
	pods     int
}

func (p *claimsWarmSustainedPhase) Name() PhaseName { return p.raw }

func (p *claimsWarmSustainedPhase) Kind() PhaseName { return PhaseClaimsWarmSustained }

func (p *claimsWarmSustainedPhase) Validate(cfg Config) error {
	// Finite checks: flag.Float64Var accepts "NaN" and "+Inf" (NaN even
	// passes a <= 0 test), and non-finite values would flow into pool
	// sizing and the capacity estimate where float->int conversion is
	// implementation-defined.
	if cfg.SustainedRate <= 0 || math.IsNaN(cfg.SustainedRate) || math.IsInf(cfg.SustainedRate, 0) {
		return fmt.Errorf("phase %q requires a finite --sustained-rate > 0", p.raw)
	}
	if cfg.SustainedSeconds <= 0 || math.IsNaN(cfg.SustainedSeconds) || math.IsInf(cfg.SustainedSeconds, 0) {
		return fmt.Errorf("phase %q requires a finite --sustained-seconds > 0", p.raw)
	}
	if cfg.SustainedNamespaces < 1 {
		return fmt.Errorf("phase %q requires --sustained-namespaces >= 1", p.raw)
	}
	if cfg.SustainedPoolHeadroom <= 0 {
		return fmt.Errorf("phase %q requires --sustained-pool-headroom > 0", p.raw)
	}
	if cfg.SustainedLifecycleBudget <= 0 {
		return fmt.Errorf("phase %q requires --sustained-lifecycle-budget > 0", p.raw)
	}
	return nil
}

func (p *claimsWarmSustainedPhase) Resolve(cfg Config, _ *ClusterInfo, resident int) (int, int) {
	// Steady state: the pools stay full (refill replaces adopted sandboxes)
	// while adopted sandboxes live for roughly the ready latency + dwell +
	// deletion pipeline before their pods go away; --sustained-lifecycle-budget
	// bounds the pipeline part on top of the dwell.
	poolTotal := sustainedPoolReplicasPerNamespace(cfg) * cfg.SustainedNamespaces
	inFlight := int(math.Ceil(cfg.SustainedRate * (cfg.ClaimDwell.Seconds() + cfg.SustainedLifecycleBudget.Seconds())))
	p.pods = poolTotal + inFlight
	// Expected arrivals; the Poisson process varies the actual count.
	p.expected = sustainedExpectedClaims(cfg)
	return resident, resident + p.pods
}

func (p *claimsWarmSustainedPhase) Description() string {
	return fmt.Sprintf("%s: ~%d claims expected, steady-state budget %d pods", p.raw, p.expected, p.pods)
}

func (p *claimsWarmSustainedPhase) Requested() int { return p.expected }

func (p *claimsWarmSustainedPhase) Run(ctx context.Context, test *stressTest, number PhaseNumber) error {
	return test.runClaimsWarmSustainedPhase(ctx, number)
}
