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
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestLatencyHelpersSkipNegativeDurations(t *testing.T) {
	t.Run("claim controller startup skips future observed time", func(t *testing.T) {
		ClaimControllerStartupLatency.Reset()
		RecordClaimControllerStartupLatency(time.Now().Add(1*time.Second), LaunchTypeCold, "test-tmpl")
		require.Equal(t, 0, testutil.CollectAndCount(ClaimControllerStartupLatency))
	})

	t.Run("sandbox creation skips negative duration", func(t *testing.T) {
		SandboxCreationLatency.Reset()
		RecordSandboxCreationLatency(-1*time.Second, "default", LaunchTypeCold, "test-tmpl")
		require.Equal(t, 0, testutil.CollectAndCount(SandboxCreationLatency))
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

// fakeTraceServer is an in-process OTLP/gRPC receiver that records export requests.
type fakeTraceServer struct {
	collectortrace.UnimplementedTraceServiceServer
	requests chan *collectortrace.ExportTraceServiceRequest
}

func (f *fakeTraceServer) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	f.requests <- req
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func TestSetupOTelExportsSpansOverOTLP(t *testing.T) {
	// No other test covers the real exporter path that --enable-tracing turns on, so a break in
	// the OTLP wiring, the service name, or trace-context continuation would go unnoticed.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fake := &fakeTraceServer{requests: make(chan *collectortrace.ExportTraceServiceRequest, 4)}
	srv := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())

	// SetupOTel replaces the process-wide provider and propagator, so restore them afterwards.
	prevProvider, prevPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})

	inst, shutdown, err := SetupOTel(context.Background(), "test-service")
	require.NoError(t, err)

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	obj := &metav1.ObjectMeta{Annotations: map[string]string{
		TraceContextAnnotation: `{"traceparent":"00-` + traceID + `-` + spanID + `-01"}`,
	}}
	ctx, end := inst.StartSpan(context.Background(), obj, "reconcile", map[string]string{"sandbox": "demo"})
	require.Contains(t, inst.GetTraceContext(ctx), traceID)
	end()
	// Shutdown flushes the batch processor.
	shutdown()

	var req *collectortrace.ExportTraceServiceRequest
	select {
	case req = <-fake.requests:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the OTLP export")
	}

	require.Len(t, req.ResourceSpans, 1)
	rs := req.ResourceSpans[0]
	var serviceName string
	for _, kv := range rs.Resource.Attributes {
		if kv.Key == "service.name" {
			serviceName = kv.Value.GetStringValue()
		}
	}
	require.Equal(t, "test-service", serviceName)

	require.Len(t, rs.ScopeSpans, 1)
	require.Len(t, rs.ScopeSpans[0].Spans, 1)
	span := rs.ScopeSpans[0].Spans[0]
	require.Equal(t, "reconcile", span.Name)
	require.Equal(t, traceID, hex.EncodeToString(span.TraceId), "span must continue the annotated trace")
	require.Equal(t, spanID, hex.EncodeToString(span.ParentSpanId))
	require.Len(t, span.Attributes, 1)
	require.Equal(t, "sandbox", span.Attributes[0].Key)
	require.Equal(t, "demo", span.Attributes[0].Value.GetStringValue())
}
