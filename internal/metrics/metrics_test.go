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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/version"
)

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

func TestStageLatencyRecording(t *testing.T) {
	SandboxStageLatency.Reset()
	RecordStageLatency(500*time.Millisecond, "default", LaunchTypeCold, "tmpl", OwnedByNone, StagePodReady)
	if testutil.CollectAndCount(SandboxStageLatency) != 1 {
		t.Errorf("Expected 1 observation for SandboxStageLatency")
	}
}

func TestChildAndTemplateReconcileErrorRecording(t *testing.T) {
	ChildReconcileErrors.Reset()
	RecordChildReconcileError("default", ResourcePod, ReasonCreateFailed)
	if testutil.CollectAndCount(ChildReconcileErrors) != 1 {
		t.Errorf("Expected 1 observation for ChildReconcileErrors")
	}

	TemplateReconcileErrors.Reset()
	RecordTemplateReconcileError("default", ReasonOwnershipConflict)
	if testutil.CollectAndCount(TemplateReconcileErrors) != 1 {
		t.Errorf("Expected 1 observation for TemplateReconcileErrors")
	}
}

func TestClassifyReconcileError(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", nil)
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", nil)

	require.Equal(t, ReasonForbidden, ClassifyReconcileError(forbidden, ReasonCreateFailed))
	require.Equal(t, ReasonUpdateConflict, ClassifyReconcileError(conflict, ReasonCreateFailed))
	require.Equal(t, ReasonCreateFailed, ClassifyReconcileError(errors.New("create boom"), ReasonCreateFailed))
	require.Equal(t, ReasonOwnershipConflict, ClassifyReconcileError(errors.New("owned by other"), ReasonOwnershipConflict))
	require.Equal(t, ReasonAdoptRefused, ClassifyReconcileError(errors.New("missing label"), ReasonAdoptRefused))
	require.Equal(t, ReasonOther, ClassifyReconcileError(errors.New("weird"), "not-a-reason"))
	require.Equal(t, ReasonOther, ClassifyReconcileError(nil, ""))
}

func TestNormalizeAllowlists(t *testing.T) {
	t.Parallel()
	require.Equal(t, StagePodReady, NormalizeStage(StagePodReady))
	require.Equal(t, ReasonOther, NormalizeStage("bogus"))
	require.Equal(t, ResourcePVC, NormalizeResource(ResourcePVC))
	require.Equal(t, ReasonOther, NormalizeResource("deployment"))
	require.Equal(t, ReasonDeleteFailed, NormalizeReason(ReasonDeleteFailed))
	require.Equal(t, ReasonOther, NormalizeReason("raw api error text"))
}

func TestStageLatencyRecordedAnnotationRoundTrip(t *testing.T) {
	t.Parallel()
	recorded := map[string]struct{}{
		StagePodReady:     {},
		StagePodCreated:   {},
		StagePodScheduled: {},
	}
	formatted := FormatStageLatencyRecorded(recorded)
	require.Equal(t, "pod_created,pod_ready,pod_scheduled", formatted)
	parsed := ParseStageLatencyRecorded(formatted + ",bogus")
	require.Equal(t, recorded, parsed)
	require.Empty(t, ParseStageLatencyRecorded(""))
}

func TestLabelsFromSandbox(t *testing.T) {
	t.Parallel()
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb",
			Namespace: "ns",
			Labels: map[string]string{
				sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
			},
			Annotations: map[string]string{
				sandboxv1beta1.SandboxTemplateRefAnnotation: "my-template",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
				Name:       "claim",
				UID:        "uid",
				Controller: new(true),
			}},
		},
	}
	labels := LabelsFromSandbox(sandbox)
	require.Equal(t, "ns", labels.Namespace)
	require.Equal(t, LaunchTypeWarm, labels.LaunchType)
	require.Equal(t, "my-template", labels.Template)
	require.Equal(t, OwnedBySandboxClaim, labels.OwnedBy)

	bare := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "ns2"}}
	bareLabels := LabelsFromSandbox(bare)
	require.Equal(t, LaunchTypeCold, bareLabels.LaunchType)
	require.Equal(t, "unknown", bareLabels.Template)
	require.Equal(t, OwnedByNone, bareLabels.OwnedBy)
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
