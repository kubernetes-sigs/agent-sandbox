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

package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// PodStatusReady indicates the pod is ready (calculated from conditions).
	PodStatusReady = "ready"
)

var (
	// WarmPoolSize is a gauge metric that monitors the point-in-time status of the warmpool.
	WarmPoolSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agent_sandbox_warmpool_size",
			Help: "Monitor the point-in-time status of the warmpool. Purpose is to be able to alert on WarmPool exhaustion.",
		},
		[]string{"pod_status", "warmpool_name", "sandbox_template"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	crmetrics.Registry.MustRegister(WarmPoolSize)
}

// UpdateWarmPoolMetrics calculates and updates the WarmPoolSize metric based on the provided pods.
func UpdateWarmPoolMetrics(wp *extensionsv1alpha1.SandboxWarmPool, pods []corev1.Pod) {
	// Initialize counts for all known phases + Ready.
	counts := map[string]float64{
		PodStatusReady: 0,
		strings.ToLower(string(corev1.PodPending)):   0,
		strings.ToLower(string(corev1.PodRunning)):   0,
		strings.ToLower(string(corev1.PodSucceeded)): 0,
		strings.ToLower(string(corev1.PodFailed)):    0,
		strings.ToLower(string(corev1.PodUnknown)):   0,
	}

	for _, pod := range pods {
		isReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}

		if isReady {
			counts[PodStatusReady]++
		} else {
			phase := strings.ToLower(string(pod.Status.Phase))
			if phase == "" {
				phase = strings.ToLower(string(corev1.PodUnknown))
			}
			if _, ok := counts[phase]; ok {
				counts[phase]++
			} else {
				counts[strings.ToLower(string(corev1.PodUnknown))]++
			}
		}
	}

	templateName := wp.Spec.TemplateRef.Name
	for status, count := range counts {
		WarmPoolSize.WithLabelValues(status, wp.Name, templateName).Set(count)
	}
}

// DeleteWarmPoolMetrics deletes all metrics for a given SandboxWarmPool.
func DeleteWarmPoolMetrics(wpName, templateName string) {
	statuses := []string{
		PodStatusReady,
		strings.ToLower(string(corev1.PodPending)),
		strings.ToLower(string(corev1.PodRunning)),
		strings.ToLower(string(corev1.PodSucceeded)),
		strings.ToLower(string(corev1.PodFailed)),
		strings.ToLower(string(corev1.PodUnknown)),
	}

	for _, status := range statuses {
		WarmPoolSize.DeleteLabelValues(status, wpName, templateName)
	}
}
