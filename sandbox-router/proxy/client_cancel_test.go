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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// TestClientCancelIsNotAnUpstreamFailure: a caller that gives up while the
// Sandbox is still working must not count or log as an upstream failure.
func TestClientCancelIsNotAnUpstreamFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The backend cancels the caller's request once it has it in hand, so the
	// cancel always lands while the Sandbox is working.
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	cfg := config.Defaults()
	cfg.UpstreamMaxRetries = 0
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"uid-1": {PodIP: backendURL.Hostname(), SandboxName: "s", Namespace: "ns"},
	}}
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	var errorLogs []string
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, `"error"=`) {
			errorLogs = append(errorLogs, args)
		}
	}, funcr.Options{})
	handler := NewHandler(Options{Config: &cfg, Cache: lookup, Metrics: metrics, Logger: logger})

	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	req.Header.Set(HeaderSandboxID, "s")
	req.Header.Set(HeaderSandboxUID, "uid-1")
	req.Header.Set(HeaderSandboxNamespace, "ns")
	req.Header.Set(HeaderSandboxPort, backendURL.Port())
	// ServeHTTP returns only after the proxy's error handler has run.
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("ns", "other")); got != 0 {
		t.Errorf(`upstream_errors_total{reason="other"} = %v, want 0`, got)
	}
	if got := testutil.ToFloat64(metrics.UpstreamErrorsTotal.WithLabelValues("ns", "client_canceled")); got != 1 {
		t.Errorf(`upstream_errors_total{reason="client_canceled"} = %v, want 1`, got)
	}
	if len(errorLogs) != 0 {
		t.Errorf("error-level logs for a client cancel: %v", errorLogs)
	}
}
