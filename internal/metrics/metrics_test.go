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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(extensionsv1alpha1.AddToScheme(scheme))
	return scheme
}

func TestMetricsRegistration(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry, fake.NewClientBuilder().WithScheme(newTestScheme()).Build())
	assert.NotNil(t, m)

	// Verify it's in the registry
	metricFamilies, err := registry.Gather()
	assert.NoError(t, err)

	found := false
	for _, mf := range metricFamilies {
		if mf.GetName() == "agent_sandbox_warmpool_size" {
			found = true
			break
		}
	}
	assert.True(t, found, "Metric agent_sandbox_warmpool_size should be registered")
}

func TestMetricsCollection(t *testing.T) {
	scheme := newTestScheme()

	pool := &extensionsv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
			UID:       "pool-uid",
		},
		Spec: extensionsv1alpha1.SandboxWarmPoolSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{
				Name: "test-template",
			},
		},
	}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					UID: "pool-uid",
				},
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-2",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					UID: "pool-uid",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(pool, pod1, pod2).
		Build()

	registry := prometheus.NewRegistry()
	_ = NewMetrics(registry, client)

	// Gather metrics
	metricFamilies, err := registry.Gather()
	assert.NoError(t, err)

	var foundCount int
	for _, mf := range metricFamilies {
		if mf.GetName() == "agent_sandbox_warmpool_size" {
			for _, metric := range mf.GetMetric() {
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
					switch status {
					case PodStatusReady:
						assert.Equal(t, 1.0, metric.GetGauge().GetValue())
						foundCount++
					case PodStatusPending:
						assert.Equal(t, 1.0, metric.GetGauge().GetValue())
						foundCount++
					case PodStatusAll:
						assert.Equal(t, 2.0, metric.GetGauge().GetValue())
						foundCount++
					}
				}
			}
		}
	}
	assert.Equal(t, 3, foundCount)
}
