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

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

func TestExtProcHandleRequestHeaders(t *testing.T) {
	var resumeCalls atomic.Int32
	mockSuspensionManager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes/resume" {
			resumeCalls.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			if err != nil {
				return
			}

			var payload map[string]string
			err = json.Unmarshal(body, &payload)
			assert.NoError(t, err)
			if err != nil {
				return
			}
			assert.Equal(t, "demo-sandbox", payload["name"])
			assert.Equal(t, "default", payload["namespace"])

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"resuming"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockSuspensionManager.Close()

	cfg := config.Defaults()
	srv := NewExtProcServer(ExtProcOptions{
		Config:               &cfg,
		SuspensionManagerURL: mockSuspensionManager.URL,
		Logger:               logr.Discard(),
	})

	headers := &extproc.HttpHeaders{
		Headers: &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{
				{Key: "X-Sandbox-ID", Value: "demo-sandbox"},
				{Key: "X-Sandbox-Namespace", Value: "default"},
			},
		},
	}

	resp := srv.handleRequestHeaders(context.Background(), headers)
	assert.NotNil(t, resp)
	assert.Equal(t, int32(1), resumeCalls.Load())
}

func TestExtProcActivityFlushing(t *testing.T) {
	var activityCalls atomic.Int32
	mockSuspensionManager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/activity" {
			activityCalls.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			if err != nil {
				return
			}

			var payload map[string]string
			err = json.Unmarshal(body, &payload)
			assert.NoError(t, err)
			if err != nil {
				return
			}
			assert.Contains(t, payload, "default/demo-sandbox")

			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockSuspensionManager.Close()

	cfg := config.Defaults()
	srv := NewExtProcServer(ExtProcOptions{
		Config:               &cfg,
		SuspensionManagerURL: mockSuspensionManager.URL,
		Logger:               logr.Discard(),
	})

	srv.RecordActivity("default/demo-sandbox")
	srv.flushActivityTimestamps(context.Background())

	assert.Equal(t, int32(1), activityCalls.Load())
}
