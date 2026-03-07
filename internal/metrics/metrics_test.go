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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
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
				t.Errorf("Expected 1 observation")
			}
		})
	}
}

func TestMetricsRegistration(t *testing.T) {
	assert.NotNil(t, WarmPoolSize)
}

func TestUpdateWarmPoolMetrics(t *testing.T) {
	pool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "test-template",
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-2",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
	}

	UpdateWarmPoolMetrics(pool, pods)

	metricChan := make(chan prometheus.Metric, 10)
	WarmPoolSize.Collect(metricChan)
	close(metricChan)

	foundReady := false
	foundPending := false
	for m := range metricChan {
		metric := &dto.Metric{}
		if err := m.Write(metric); err != nil {
			t.Fatalf("failed to write metric: %v", err)
		}

		var status, poolName, template string
		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "pod_status":
				status = label.GetValue()
			case "warmpool_name":
				poolName = label.GetValue()
			case "sandbox_template":
				template = label.GetValue()
			}
		}

		if poolName == "test-pool" && template == "test-template" {
			if status == PodStatusReady {
				assert.Equal(t, 1.0, metric.GetGauge().GetValue())
				foundReady = true
			}
			if status == strings.ToLower(string(corev1.PodPending)) {
				assert.Equal(t, 1.0, metric.GetGauge().GetValue())
				foundPending = true
			}
			// Verify that an aggregate count label like "*" does NOT exist
			assert.NotEqual(t, "*", status)
		}
	}

	assert.True(t, foundReady)
	assert.True(t, foundPending)
}
