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

package observability

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// collectCounterValue reads the current value of a counter with the given
// label values from a CounterVec. Returns 0 if the series does not exist.
func collectCounterValue(t *testing.T, cv *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := cv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", lvs, err)
	}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// collectHistogramCount reads the sample count from a histogram with the
// given label values. Returns 0 if the series does not exist.
func collectHistogramCount(t *testing.T, hv *prometheus.HistogramVec, lvs ...string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	h, err := hv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", lvs, err)
	}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestMiddleware_WritePath tests that the middleware correctly counts
// bytes written through the standard Write path.
func TestMiddleware_WritePath(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	body := "hello, world"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("Write: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	m.Middleware(inner).ServeHTTP(rec, req)

	if got := rec.Body.String(); got != body {
		t.Errorf("response body: got %q, want %q", got, body)
	}

	txBytes := collectCounterValue(t, m.ClientTxBytesTotal, "test-ns")
	if txBytes != float64(len(body)) {
		t.Errorf("ClientTxBytesTotal: got %v, want %v", txBytes, len(body))
	}

	histCount := collectHistogramCount(t, m.ResponseSizeBytes, "test-ns")
	if histCount != 1 {
		t.Errorf("ResponseSizeBytes sample count: got %v, want 1", histCount)
	}
}

// readerFromWriter is a ResponseWriter that implements io.ReaderFrom to
// test the delegate path in statusRecorder.ReadFrom.
type readerFromWriter struct {
	http.ResponseWriter
	readFromCalled bool
	readFromBytes  int64
}

func (w *readerFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFromCalled = true
	buf := make([]byte, 1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			nw, _ := w.Write(buf[:n])
			total += int64(nw)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return total, err
		}
	}
	w.readFromBytes = total
	return total, nil
}

// TestMiddleware_ReadFromDelegate tests that when the underlying
// ResponseWriter implements io.ReaderFrom, the middleware delegates to
// it and still counts bytes correctly.
func TestMiddleware_ReadFromDelegate(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	body := strings.Repeat("x", 4096)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Use a reader that does NOT implement io.WriterTo, so io.Copy
		// will prefer the destination's io.ReaderFrom if available.
		// bytes.Reader implements WriterTo, so we wrap it.
		reader := struct{ io.Reader }{bytes.NewReader([]byte(body))}
		if _, err := io.Copy(w, reader); err != nil {
			t.Errorf("io.Copy: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)

	// Use a custom recorder that implements io.ReaderFrom.
	// We wrap a basic ResponseWriter to avoid httptest.Recorder's own
	// ReadFrom implementation interfering.
	baseWriter := httptest.NewRecorder()
	rfw := &readerFromWriter{ResponseWriter: baseWriter}

	m.Middleware(inner).ServeHTTP(rfw, req)

	if !rfw.readFromCalled {
		t.Error("expected ReadFrom to be called on the underlying ResponseWriter")
	}

	txBytes := collectCounterValue(t, m.ClientTxBytesTotal, "test-ns")
	if txBytes != float64(len(body)) {
		t.Errorf("ClientTxBytesTotal: got %v, want %v", txBytes, len(body))
	}
}

// TestMiddleware_ReadFromFallback tests that when the underlying
// ResponseWriter does NOT implement io.ReaderFrom, the middleware falls
// back to copying through its own Write method.
func TestMiddleware_ReadFromFallback(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	body := "fallback test data"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// io.Copy should fall back to Write since plainResponseWriter
		// does not implement io.ReaderFrom.
		if _, err := io.Copy(w, strings.NewReader(body)); err != nil {
			t.Errorf("io.Copy: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)

	// plainResponseWriter wraps httptest.ResponseWriter but explicitly
	// does not implement io.ReaderFrom (we use an interface that hides it).
	rec := httptest.NewRecorder()
	plain := struct{ http.ResponseWriter }{rec}

	m.Middleware(inner).ServeHTTP(plain, req)

	txBytes := collectCounterValue(t, m.ClientTxBytesTotal, "test-ns")
	if txBytes != float64(len(body)) {
		t.Errorf("ClientTxBytesTotal: got %v, want %v", txBytes, len(body))
	}
}

// TestMiddleware_RequestBodyCounting tests that request body bytes are
// counted correctly through the countingReadCloser wrapper.
func TestMiddleware_RequestBodyCounting(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	reqBody := "request body content"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the entire body.
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		if string(data) != reqBody {
			t.Errorf("body content: got %q, want %q", string(data), reqBody)
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(reqBody)))
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	m.Middleware(inner).ServeHTTP(rec, req)

	rxBytes := collectCounterValue(t, m.ClientRxBytesTotal, "test-ns")
	if rxBytes != float64(len(reqBody)) {
		t.Errorf("ClientRxBytesTotal: got %v, want %v", rxBytes, len(reqBody))
	}

	histCount := collectHistogramCount(t, m.RequestSizeBytes, "test-ns")
	if histCount != 1 {
		t.Errorf("RequestSizeBytes sample count: got %v, want 1", histCount)
	}
}

// TestMiddleware_NoBodySkipsMetrics verifies that requests with no body
// (GET/HEAD) do not record zero-byte observations for request metrics.
func TestMiddleware_NoBodySkipsMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	m.Middleware(inner).ServeHTTP(rec, req)

	// ClientRxBytesTotal should not have a series for this namespace
	// since no body was present.
	histCount := collectHistogramCount(t, m.RequestSizeBytes, "test-ns")
	if histCount != 0 {
		t.Errorf("RequestSizeBytes sample count: got %v, want 0 (no body should not record)", histCount)
	}
}

// TestMiddleware_HttpNoBodySkipped verifies that http.NoBody is explicitly
// skipped and does not record metrics.
func TestMiddleware_HttpNoBodySkipped(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != http.NoBody {
			t.Errorf("expected r.Body to be http.NoBody, got %T", r.Body)
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Ensure Body is http.NoBody (httptest.NewRequest should do this).
	if req.Body == nil {
		req.Body = http.NoBody
	}
	ctx := WithLabels(req.Context(), &Labels{SandboxNamespace: "test-ns"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	m.Middleware(inner).ServeHTTP(rec, req)

	histCount := collectHistogramCount(t, m.RequestSizeBytes, "test-ns")
	if histCount != 0 {
		t.Errorf("RequestSizeBytes sample count: got %v, want 0 (http.NoBody should not record)", histCount)
	}
}

// TestStatusRecorder_ReadFromSetsStatus verifies that the ReadFrom
// delegate path correctly sets the status to 200 when no explicit
// WriteHeader was called.
func TestStatusRecorder_ReadFromSetsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	body := "test body"
	n, err := s.ReadFrom(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("ReadFrom bytes: got %d, want %d", n, len(body))
	}
	if s.bytesWritten != int64(len(body)) {
		t.Errorf("bytesWritten: got %d, want %d", s.bytesWritten, len(body))
	}
	if !s.wroteHeader {
		t.Error("expected wroteHeader to be true after ReadFrom with n > 0")
	}
	if s.status != http.StatusOK {
		t.Errorf("status: got %d, want %d", s.status, http.StatusOK)
	}
}

// TestStatusRecorder_ReadFromPreservesExplicitStatus verifies that an
// explicit WriteHeader before ReadFrom is preserved.
func TestStatusRecorder_ReadFromPreservesExplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	s.WriteHeader(http.StatusCreated)

	body := "test body"
	if _, err := s.ReadFrom(strings.NewReader(body)); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if s.status != http.StatusCreated {
		t.Errorf("status: got %d, want %d (explicit status should be preserved)", s.status, http.StatusCreated)
	}
}
