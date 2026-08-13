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

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func setupScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1beta1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func TestSuspensionServerResumeHandler_AdministrativelySuspended(t *testing.T) {
	scheme := setupScheme(t)

	sandbox := &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "suspended-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: agentsv1beta1.SandboxOperatingModeSuspended,
			SandboxBlueprint: agentsv1beta1.SandboxBlueprint{
				PodTemplate: agentsv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
					},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	payload := map[string]string{
		"name":      "suspended-sandbox",
		"namespace": "default",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/resume", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "cannot resume sandbox with spec.operatingMode set to Suspended")

	var updated agentsv1beta1.Sandbox
	err := client.Get(context.Background(), types.NamespacedName{Name: "suspended-sandbox", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, agentsv1beta1.SandboxOperatingModeSuspended, updated.Spec.OperatingMode)
	assert.Nil(t, updated.Status.LastActivityTime)
}

func TestSuspensionServerResumeHandler_Running(t *testing.T) {
	scheme := setupScheme(t)

	sandbox := &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: agentsv1beta1.SandboxOperatingModeRunning,
			SandboxBlueprint: agentsv1beta1.SandboxBlueprint{
				PodTemplate: agentsv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
					},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	payload := map[string]string{
		"name":      "running-sandbox",
		"namespace": "default",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/resume", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated agentsv1beta1.Sandbox
	err := client.Get(context.Background(), types.NamespacedName{Name: "running-sandbox", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, agentsv1beta1.SandboxOperatingModeRunning, updated.Spec.OperatingMode)
	assert.NotNil(t, updated.Status.LastActivityTime)
}

func TestSuspensionServerResumeForAutoSuspended(t *testing.T) {
	scheme := setupScheme(t)

	oldTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	timeout := int32(300)

	sandbox := &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "auto-suspended-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: agentsv1beta1.SandboxOperatingModeRunning,
			Lifecycle: agentsv1beta1.Lifecycle{
				AutoSuspension: &agentsv1beta1.AutoSuspensionPolicy{
					InactivityTimeoutSeconds: &timeout,
				},
			},
			SandboxBlueprint: agentsv1beta1.SandboxBlueprint{
				PodTemplate: agentsv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
					},
				},
			},
		},
		Status: agentsv1beta1.SandboxStatus{
			LastActivityTime: &oldTime,
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	payload := map[string]string{
		"name":      "auto-suspended-sandbox",
		"namespace": "default",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/resume", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated agentsv1beta1.Sandbox
	err := client.Get(context.Background(), types.NamespacedName{Name: "auto-suspended-sandbox", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, agentsv1beta1.SandboxOperatingModeRunning, updated.Spec.OperatingMode)
	assert.NotNil(t, updated.Status.LastActivityTime)
	assert.True(t, updated.Status.LastActivityTime.Time.After(oldTime.Time), "lastActivityTime must be refreshed to now()")
}

func TestSuspensionServerActivityHandler(t *testing.T) {
	scheme := setupScheme(t)

	sandbox := &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "active-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: agentsv1beta1.SandboxOperatingModeRunning,
			SandboxBlueprint: agentsv1beta1.SandboxBlueprint{
				PodTemplate: agentsv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
					},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	nowStr := time.Now().Format(time.RFC3339)
	payload := map[string]string{
		"default/active-sandbox": nowStr,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/activity", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated agentsv1beta1.Sandbox
	err := client.Get(context.Background(), types.NamespacedName{Name: "active-sandbox", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.NotNil(t, updated.Status.LastActivityTime)
}

func TestSuspensionServerActivityHandlerIgnoresNotFoundAndInvalidKeys(t *testing.T) {
	scheme := setupScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	payload := map[string]string{
		"default/nonexistent-sandbox": time.Now().Format(time.RFC3339),
		"invalid-format-key":          time.Now().Format(time.RFC3339),
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/activity", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSuspensionServerActivityHandlerClampsFutureTimestamp(t *testing.T) {
	scheme := setupScheme(t)

	sandbox := &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "future-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: agentsv1beta1.SandboxOperatingModeRunning,
			SandboxBlueprint: agentsv1beta1.SandboxBlueprint{
				PodTemplate: agentsv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
					},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(sandbox).WithObjects(sandbox).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	futureStr := "2099-01-01T00:00:00Z"
	payload := map[string]string{
		"default/future-sandbox": futureStr,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/activity", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated agentsv1beta1.Sandbox
	err := client.Get(context.Background(), types.NamespacedName{Name: "future-sandbox", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.NotNil(t, updated.Status.LastActivityTime)
	assert.True(t, updated.Status.LastActivityTime.Time.Before(time.Now().Add(time.Second)), "timestamp should be clamped to current time, not year 2099")
}

func TestSuspensionServerActivityHandlerRejectsOversizedPayload(t *testing.T) {
	scheme := setupScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := NewSuspensionServer(client, logr.Discard())

	payload := make(map[string]string)
	nowStr := time.Now().Format(time.RFC3339)
	for i := 0; i < maxActivityEntries+1; i++ {
		payload[fmt.Sprintf("default/sb-%d", i)] = nowStr
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/activity", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
