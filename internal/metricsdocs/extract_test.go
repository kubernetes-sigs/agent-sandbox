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

package metricsdocs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

func sourceDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

func promSource(declarations string) string {
	return "package fake\n\nimport \"github.com/prometheus/client_golang/prometheus\"\n\n" + declarations + "\n"
}

func extractDeclarations(t *testing.T, declarations string) ([]family, error) {
	t.Helper()
	return extract(sourceDir(t, map[string]string{"metrics.go": promSource(declarations)}))
}

func TestExtractSupportedDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		declarations string
		want         []family
	}{
		{
			name: "histogram vector ignores buckets",
			declarations: `var Latency = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "sandbox_latency_ms",
		Help:    "Latency in milliseconds.",
		Buckets: []float64{100, 250},
	},
	[]string{"launch_type", "sandbox_template"},
)`,
			want: []family{{
				Name:   "sandbox_latency_ms",
				Type:   typeHistogram,
				Help:   "Latency in milliseconds.",
				Labels: []string{"launch_type", "sandbox_template"},
			}},
		},
		{
			name: "concatenated help string is joined",
			declarations: `var Latency = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "sandbox_latency_ms",
		Help: "First part. " +
			"Second part. " +
			"Third part.",
	},
	[]string{"launch_type"},
)`,
			want: []family{{
				Name:   "sandbox_latency_ms",
				Type:   typeHistogram,
				Help:   "First part. Second part. Third part.",
				Labels: []string{"launch_type"},
			}},
		},
		{
			name: "namespace and subsystem build the full name",
			declarations: `var Gauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "agent",
		Subsystem: "sandbox",
		Name:      "active",
		Help:      "Active sandboxes.",
	},
	[]string{"namespace"},
)`,
			want: []family{{
				Name:   "agent_sandbox_active",
				Type:   typeGauge,
				Help:   "Active sandboxes.",
				Labels: []string{"namespace"},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractDeclarations(t, tc.declarations)
			require.NoError(t, err)
			require.Empty(t, cmp.Diff(tc.want, got, cmpopts.EquateEmpty()))
		})
	}
}

func TestExtractRejectsUnsupportedDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		declarations string
		wantErr      string
	}{
		{
			// The realistic shape: a shared prefix constant, which reads like a
			// literal but is an identifier the extractor cannot resolve.
			name: "name built from a constant",
			declarations: `const prefix = "agent_sandbox_"

var M = prometheus.NewCounter(prometheus.CounterOpts{Name: prefix + "claims_total", Help: "H."})`,
			wantErr: "expected a string literal",
		},
		{
			// buildFQName is a local reimplementation of prometheus.BuildFQName
			// and, unlike upstream, happily joins a namespace and subsystem into
			// a plausible name for a metric that has none. Prometheus would
			// refuse to register this.
			name:         "namespace and subsystem but no name",
			declarations: `var M = prometheus.NewCounter(prometheus.CounterOpts{Namespace: "agent", Subsystem: "sandbox", Help: "H."})`,
			wantErr:      "options must set Name to a string literal",
		},
		{
			name:         "no help text",
			declarations: `var M = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total"})`,
			wantErr:      "metric m_total has no help text",
		},
		{
			// Three real histograms share an identical bucket slice, so folding
			// their options into a helper is the obvious refactor.
			name: "options from a helper call",
			declarations: `func latencyOpts(name, help string) prometheus.HistogramOpts {
	return prometheus.HistogramOpts{Name: name, Help: help}
}

var M = prometheus.NewHistogramVec(latencyOpts("m_ms", "H."), []string{"launch_type"})`,
			wantErr: "options must be a composite literal",
		},
		{
			name: "variable label slice from a variable",
			declarations: `var labelNames = []string{"launch_type"}

var M = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "m_total", Help: "H."}, labelNames)`,
			wantErr: "expected a []string literal",
		},
		{
			name: "non literal const label key",
			declarations: `const versionKey = "git_version"

var M = prometheus.NewGauge(prometheus.GaugeOpts{Name: "m", Help: "H.", ConstLabels: prometheus.Labels{versionKey: "v"}})`,
			wantErr: "expected a string literal",
		},
		{
			name:         "descriptor without a const metric call",
			declarations: `var Desc = prometheus.NewDesc("m", "H.", []string{"namespace"}, nil)`,
			wantErr:      "cannot determine the type of descriptor Desc",
		},
		{
			// prometheus.ValueType is int-based, so an untyped constant of the
			// same name elsewhere is assignable and only the qualifier tells
			// the two apart.
			name: "descriptor value type from another package",
			declarations: `var Desc = prometheus.NewDesc("m", "H.", nil, nil)

func collect() prometheus.Metric {
	return prometheus.MustNewConstMetric(Desc, other.GaugeValue, 1)
}`,
			wantErr: "value type must be a prometheus constant",
		},
		{
			// The other arm of the same guard: not a selector at all, which is
			// what hoisting the value type into a variable produces.
			name: "descriptor value type from a variable",
			declarations: `var Desc = prometheus.NewDesc("m", "H.", nil, nil)

func collect(valueType prometheus.ValueType) prometheus.Metric {
	return prometheus.MustNewConstMetric(Desc, valueType, 1)
}`,
			wantErr: "value type must be a prometheus constant",
		},
		{
			name: "descriptor used with conflicting types",
			declarations: `var Desc = prometheus.NewDesc("m", "H.", nil, nil)

func collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(Desc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(Desc, prometheus.CounterValue, 1)
}`,
			wantErr: "used as both a Gauge and a Counter",
		},
		{
			name: "constructor outside a package-level var",
			declarations: `func newCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})
}`,
			wantErr: "NewCounter must be the value of a package-level var to be documented",
		},
		{
			// A constructor this extractor has never heard of must not slip
			// through. NewProcessCollector is a real one a contributor could
			// plausibly add to namespace the process metrics.
			name:         "unknown constructor taking metric options",
			declarations: `var M = prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{Namespace: "agent_sandbox"})`,
			wantErr:      "NewProcessCollector is not a metric constructor this generator knows",
		},
		{
			// The call goes through newCounter, which carries no Prometheus
			// qualifier, so the reference is the only thing left to refuse.
			name: "constructor referenced as a value",
			declarations: `var newCounter = prometheus.NewCounter

var M = newCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})`,
			wantErr: "prometheus.NewCounter is referenced as a value",
		},
		{
			// A constructor that never passes through a prometheus selector,
			// so the options literal is all that marks the call as a metric.
			name: "options passed to an unresolvable call",
			declarations: `func register(construct func(prometheus.CounterOpts) prometheus.Counter) {
	_ = construct(prometheus.CounterOpts{Name: "m_total", Help: "H."})
}`,
			wantErr: "metric options are passed to a call this generator cannot resolve",
		},
		{
			// A registry or logger ahead of the options is the shape a name
			// list would miss, so the options are looked for in any position.
			name:         "unknown constructor taking metric options after another argument",
			declarations: `var M = prometheus.NewSomethingV3(registry, prometheus.CounterOpts{Name: "m_total", Help: "H."})`,
			wantErr:      "NewSomethingV3 is not a metric constructor this generator knows",
		},
		{
			// prometheus.V2 is the upstream route to constrained labels, and
			// reads as an ordinary constructor call.
			name: "constructor reached through the V2 namespace",
			declarations: `var M = prometheus.V2.NewHistogramVec(
	prometheus.HistogramVecOpts{
		HistogramOpts: prometheus.HistogramOpts{Name: "m_ms", Help: "H."},
	},
)`,
			wantErr: "V2.NewHistogramVec is not a metric constructor this generator reads",
		},
		{
			name:         "invalid metric name",
			declarations: `var M = prometheus.NewCounter(prometheus.CounterOpts{Name: "not-a-metric", Help: "H."})`,
			wantErr:      `metrics.go:5:9: invalid metric name "not-a-metric"`,
		},
		{
			// A colon is legal in a metric name and illegal in a label name,
			// which is the only place the two patterns actually differ.
			name:         "invalid label name",
			declarations: `var M = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agent:sandbox:ratio", Help: "H."}, []string{"launch:type"})`,
			wantErr:      `invalid label name "launch:type"`,
		},
		{
			name: "duplicate metric name",
			declarations: `var (
	First  = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "First."})
	Second = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "Second."})
)`,
			wantErr: `metric m_total is already declared at`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractDeclarations(t, tc.declarations)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestExtractRejectsPromautoImports pins the refusal at the import, since
// promauto constructors take prometheus Opts and so the file imports both
// packages, leaving the import path as the only signal.
func TestExtractRejectsPromautoImports(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go": "package fake\n\nimport (\n" +
			"\t\"github.com/prometheus/client_golang/prometheus\"\n" +
			"\t\"github.com/prometheus/client_golang/prometheus/promauto\"\n)\n\n" +
			`var M = promauto.NewCounterVec(prometheus.CounterOpts{Name: "m_total", Help: "H."}, []string{"namespace"})` + "\n",
	})

	_, err := extract(dir)
	require.ErrorContains(t, err, "importing github.com/prometheus/client_golang/prometheus/promauto is not supported")
}

// TestExtractRejectsBuildConstraints carries the license header every file in
// this repository starts with, so the directive is checked against the comment
// layout the generator will actually meet rather than a bare fixture.
func TestExtractRejectsBuildConstraints(t *testing.T) {
	directives := []struct {
		name      string
		directive string
	}{
		{"go:build directive", "//go:build !race"},
		{"legacy +build directive", "// +build !race"},
	}

	for _, tc := range directives {
		t.Run(tc.name, func(t *testing.T) {
			dir := sourceDir(t, map[string]string{
				"metrics.go": "// Copyright 2026 The Kubernetes Authors.\n\n" + tc.directive + "\n\n" +
					promSource(`var M = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})`),
			})

			_, err := extract(dir)
			require.ErrorContains(t, err, "build constraints are not supported")
		})
	}
}

// TestExtractIgnoresUnrelatedBuildConstraints guards against the check above
// failing the autogen presubmit: a platform-specific file that declares no
// metrics is none of the generator's business.
func TestExtractIgnoresUnrelatedBuildConstraints(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go":     promSource(`var Total = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})`),
		"debug_linux.go": "//go:build linux\n\npackage fake\n\nconst platform = \"linux\"\n",
	})

	got, err := extract(dir)
	require.NoError(t, err)
	require.Equal(t, []family{{Name: "m_total", Type: typeCounter, Help: "H."}}, got)
}

func TestExtractSkipsFilesWithoutMetrics(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go": promSource(`var Total = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})`),
		// Mirrors internal/metrics/tracing.go: same package, no metrics.
		"tracing.go": "package fake\n\nimport \"go.opentelemetry.io/otel/trace\"\n\nvar Tracer trace.Tracer\n",
		// Test files never contribute to the published reference.
		"metrics_test.go": promSource(`var Hidden = prometheus.NewCounter(prometheus.CounterOpts{Name: "hidden_total", Help: "H."})`),
		// Nor do the two filename shapes the toolchain ignores outright.
		"_scratch.go": promSource(`var AlsoHidden = prometheus.NewCounter(prometheus.CounterOpts{Name: "also_hidden_total", Help: "H."})`),
		".scratch.go": promSource(`var StillHidden = prometheus.NewCounter(prometheus.CounterOpts{Name: "still_hidden_total", Help: "H."})`),
	})

	got, err := extract(dir)
	require.NoError(t, err)
	require.Equal(t, []family{{Name: "m_total", Type: typeCounter, Help: "H."}}, got)
}

func TestExtractReadsAliasedImports(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go": "package fake\n\nimport prom \"github.com/prometheus/client_golang/prometheus\"\n\n" +
			`var Total = prom.NewCounter(prom.CounterOpts{Name: "m_total", Help: "H."})` + "\n",
	})

	got, err := extract(dir)
	require.NoError(t, err)
	require.Equal(t, []family{{Name: "m_total", Type: typeCounter, Help: "H."}}, got)
}

func TestExtractRejectsDotImports(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go": "package fake\n\nimport . \"github.com/prometheus/client_golang/prometheus\"\n\n" +
			`var Total = NewCounter(CounterOpts{Name: "m_total", Help: "H."})` + "\n",
	})

	_, err := extract(dir)
	require.ErrorContains(t, err, "dot import of github.com/prometheus/client_golang/prometheus is not supported")
}

func TestExtractRejectsDuplicateImports(t *testing.T) {
	dir := sourceDir(t, map[string]string{
		"metrics.go": "package fake\n\nimport (\n" +
			"\t\"github.com/prometheus/client_golang/prometheus\"\n" +
			"\t_ \"github.com/prometheus/client_golang/prometheus\"\n)\n\n" +
			`var M = prometheus.NewCounter(prometheus.CounterOpts{Name: "m_total", Help: "H."})` + "\n",
	})

	_, err := extract(dir)
	require.ErrorContains(t, err, "is imported more than once")
}
