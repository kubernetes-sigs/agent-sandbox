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

// nolint:revive
package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/version"
)

func histogramSampleCount(vec *prometheus.HistogramVec) uint64 {
	ch := make(chan prometheus.Metric)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var count uint64
	for m := range ch {
		pb := &io_prometheus_client.Metric{}
		if err := m.Write(pb); err == nil {
			count += pb.GetHistogram().GetSampleCount()
		}
	}
	return count
}

func TestClaimLatencyRecording(t *testing.T) {
	testCases := []struct {
		name       string
		launchType string
	}{
		{"Warm", LaunchTypeWarm},
		{"Cold", LaunchTypeCold},
		{"Unknown", LaunchTypeUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ClaimStartupLatency.Reset()
			ClaimStartupLatency.WithLabelValues(tc.launchType, "test-tmpl").Observe(1000)

			if testutil.CollectAndCount(ClaimStartupLatency) != 1 {
				t.Errorf("Expected 1 observation for ClaimStartupLatency")
			}

			ClaimControllerStartupLatency.Reset()
			ClaimControllerStartupLatency.WithLabelValues(tc.launchType, "test-tmpl").Observe(1000)

			if testutil.CollectAndCount(ClaimControllerStartupLatency) != 1 {
				t.Errorf("Expected 1 observation for ClaimControllerStartupLatency")
			}
		})
	}
}

func TestClientClaimLatencyRecording(t *testing.T) {
	ctx := context.Background()
	testCases := []struct {
		name       string
		launchType string
	}{
		{"Warm", LaunchTypeWarm},
		{"Cold", LaunchTypeCold},
		{"Unknown", LaunchTypeUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ClientClaimStartupLatency.Reset()
			RecordClientClaimStartupLatency(ctx, time.Now().Add(-1*time.Second), tc.launchType, "test-tmpl")

			if testutil.CollectAndCount(ClientClaimStartupLatency) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestSandboxCreationLatencyRecording(t *testing.T) {
	testCases := []struct {
		name       string
		launchType string
	}{
		{"Warm", LaunchTypeWarm},
		{"Cold", LaunchTypeCold},
		{"Unknown", LaunchTypeUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			SandboxCreationLatency.Reset()
			RecordSandboxCreationLatency(1000*time.Millisecond, "default", tc.launchType, "test-tmpl")

			if testutil.CollectAndCount(SandboxCreationLatency) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestSandboxLifecycleMetricsRecording(t *testing.T) {
	t.Run("SuspendTotal", func(t *testing.T) {
		SandboxSuspendTotal.Reset()
		RecordSandboxSuspend("default", "tmpl-a", extensionsv1beta1.SandboxClaimKind, SuspendResultRequested)
		RecordSandboxSuspend("default", "tmpl-a", extensionsv1beta1.SandboxClaimKind, SuspendResultPodTerminated)
		RecordSandboxSuspend("default", "", "invalid-owner", "unexpected-result")

		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxSuspendTotal.WithLabelValues("default", "tmpl-a", "SandboxClaim", "requested")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxSuspendTotal.WithLabelValues("default", "tmpl-a", "SandboxClaim", "pod_terminated")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxSuspendTotal.WithLabelValues("default", "unknown", "None", "unknown")), 0.001)
	})

	t.Run("SuspendLatency", func(t *testing.T) {
		SandboxSuspendLatency.Reset()
		RecordSandboxSuspendLatency(1500*time.Millisecond, "default", "tmpl-a", extensionsv1beta1.SandboxClaimKind)
		RecordSandboxSuspendLatency(2500*time.Millisecond, "default", "tmpl-a", extensionsv1beta1.SandboxClaimKind)

		require.Equal(t, uint64(2), histogramSampleCount(SandboxSuspendLatency))
	})

	t.Run("ResumeTotal", func(t *testing.T) {
		SandboxResumeTotal.Reset()
		RecordSandboxResume("default", "tmpl-a", extensionsv1beta1.SandboxClaimKind, ResumeResultRequested)
		RecordSandboxResume("default", "tmpl-a", extensionsv1beta1.SandboxClaimKind, ResumeResultReady)
		RecordSandboxResume("default", "", "custom", "bad")

		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxResumeTotal.WithLabelValues("default", "tmpl-a", "SandboxClaim", "requested")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxResumeTotal.WithLabelValues("default", "tmpl-a", "SandboxClaim", "ready")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxResumeTotal.WithLabelValues("default", "unknown", "None", "unknown")), 0.001)
	})

	t.Run("ResumeLatency", func(t *testing.T) {
		SandboxResumeLatency.Reset()
		RecordSandboxResumeLatency(2*time.Second, "default", "tmpl-a", extensionsv1beta1.SandboxClaimKind)
		RecordSandboxResumeLatency(3*time.Second, "default", "tmpl-a", extensionsv1beta1.SandboxClaimKind)

		require.Equal(t, uint64(2), histogramSampleCount(SandboxResumeLatency))
	})

	t.Run("FinishedTotal", func(t *testing.T) {
		SandboxFinishedTotal.Reset()
		RecordSandboxFinished("default", "tmpl-a", OwnedByNone, sandboxv1beta1.SandboxReasonPodSucceeded)
		RecordSandboxFinished("default", "tmpl-a", OwnedByNone, sandboxv1beta1.SandboxReasonPodFailed)
		RecordSandboxFinished("default", "", "other", "UnknownReason")

		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxFinishedTotal.WithLabelValues("default", "tmpl-a", "None", "PodSucceeded")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxFinishedTotal.WithLabelValues("default", "tmpl-a", "None", "PodFailed")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxFinishedTotal.WithLabelValues("default", "unknown", "None", "unknown")), 0.001)
	})

	t.Run("ExpiredTotal", func(t *testing.T) {
		SandboxExpiredTotal.Reset()
		RecordSandboxExpired("default", "tmpl-a", extensionsv1beta1.SandboxClaimKind, string(sandboxv1beta1.ShutdownPolicyDelete))
		RecordSandboxExpired("default", "", "other", "")

		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxExpiredTotal.WithLabelValues("default", "tmpl-a", "SandboxClaim", "Delete")), 0.001)
		require.InDelta(t, 1.0, testutil.ToFloat64(SandboxExpiredTotal.WithLabelValues("default", "unknown", "None", "Retain")), 0.001)
	})

	t.Run("SandboxLabelExtractors", func(t *testing.T) {
		require.Equal(t, "unknown", SandboxTemplateLabel(nil))
		require.Equal(t, "None", SandboxOwnedByLabel(nil))
		require.Equal(t, "Retain", SandboxShutdownPolicyLabel(nil))

		delPolicy := sandboxv1beta1.ShutdownPolicyDelete
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					sandboxv1beta1.SandboxTemplateRefAnnotation: "my-template",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: extensionsv1beta1.GroupVersion.String(),
						Kind:       extensionsv1beta1.SandboxClaimKind,
						Name:       "my-claim",
						Controller: new(true),
					},
				},
			},
			Spec: sandboxv1beta1.SandboxSpec{
				Lifecycle: sandboxv1beta1.Lifecycle{
					ShutdownPolicy: &delPolicy,
				},
			},
		}
		require.Equal(t, "my-template", SandboxTemplateLabel(sb))
		require.Equal(t, "SandboxClaim", SandboxOwnedByLabel(sb))
		require.Equal(t, "Delete", SandboxShutdownPolicyLabel(sb))
	})
}

func TestSandboxClaimCreationRecording(t *testing.T) {
	testCases := []struct {
		name         string
		launchType   string
		podCondition string
	}{
		{"WarmReady", LaunchTypeWarm, "ready"},
		{"WarmNotReady", LaunchTypeWarm, "not_ready"},
		{"Cold", LaunchTypeCold, "not_ready"},
		{"Unknown", LaunchTypeUnknown, "not_ready"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			SandboxClaimCreationTotal.Reset()
			SandboxClaimCreationTotal.WithLabelValues("default", "test-tmpl", tc.launchType, "test-pool", tc.podCondition, "unknown").Inc()

			if testutil.CollectAndCount(SandboxClaimCreationTotal) != 1 {
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestBuildInfo(t *testing.T) {
	expected := strings.TrimSpace(`
		# HELP agent_sandbox_build_info Agent sandbox controller build metadata exposed as labels with a constant value of 1.
		# TYPE agent_sandbox_build_info gauge
		agent_sandbox_build_info{build_date="`+version.Get().BuildDate+`",compiler="`+version.Get().Compiler+`",git_commit="`+version.Get().GitSHA+`",git_version="`+version.Get().GitVersion+`",go_version="`+version.Get().GoVersion+`",platform="`+version.Get().Platform+`"} 1
	`) + "\n"

	if err := testutil.CollectAndCompare(BuildInfo, strings.NewReader(expected)); err != nil {
		t.Errorf("BuildInfo metric mismatch: %v", err)
	}
}

func TestStartSpanEndFuncEndsSpan(t *testing.T) {
	// StartSpan returns an end func; if the caller never invokes it, span.End is never called and
	// the span is never exported, a span resource leak. This mini test just proves the func closes the span.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	inst := &otelInstrumenter{
		tracer:     tp.Tracer("test"),
		propagator: propagation.TraceContext{},
		logger:     logr.Discard(),
	}

	_, end := inst.StartSpan(context.Background(), nil, "op", nil)
	end()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.False(t, spans[0].EndTime.IsZero(), "end func must call span.End")
}
