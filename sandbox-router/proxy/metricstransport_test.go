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
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

// upgradedBody mimics the body http.Transport returns for a 101 response.
type upgradedBody struct {
	io.Reader
	written bytes.Buffer
}

func (b *upgradedBody) Write(p []byte) (int, error) { return b.written.Write(p) }
func (b *upgradedBody) Close() error                { return nil }

func TestMetricsTransport_KeepsUpgradedBodyWritable(t *testing.T) {
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	body := &upgradedBody{Reader: strings.NewReader("from-upstream")}
	tr := newMetricsTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: body}, nil
	}), metrics)

	resp, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// httputil.ReverseProxy fails a 101 with 502 unless the body is an io.ReadWriteCloser.
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("101 body is %T, want io.ReadWriteCloser", resp.Body)
	}
	if _, err := io.Copy(io.Discard, rwc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := rwc.Write([]byte("to-upstream")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := body.written.String(); got != "to-upstream" {
		t.Errorf("upstream received %q, want %q", got, "to-upstream")
	}
	if got := testutil.ToFloat64(metrics.UpstreamRxBytesTotal.WithLabelValues("-")); got != float64(len("from-upstream")) {
		t.Errorf("rx bytes: got %v, want %d", got, len("from-upstream"))
	}
	if got := testutil.ToFloat64(metrics.UpstreamTxBytesTotal.WithLabelValues("-")); got != float64(len("to-upstream")) {
		t.Errorf("tx bytes: got %v, want %d", got, len("to-upstream"))
	}
}

func TestMetricsTransport_PlainBodyStaysReadOnly(t *testing.T) {
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	tr := newMetricsTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("hello"))}, nil
	}), metrics)

	resp, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, ok := resp.Body.(io.Writer); ok {
		t.Errorf("plain response body %T must not be writable", resp.Body)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := testutil.ToFloat64(metrics.UpstreamRxBytesTotal.WithLabelValues("-")); got != float64(len("hello")) {
		t.Errorf("rx bytes: got %v, want %d", got, len("hello"))
	}
}
