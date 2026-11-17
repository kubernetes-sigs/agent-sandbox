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

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRunner(t *testing.T, handle func(ctx context.Context, req WorkRequest) error) (*Runner, *httptest.Server) {
	t.Helper()
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r.addr != ":8080" {
		t.Errorf("default addr = %q, want :8080", r.addr)
	}
	r.handleItem = handle
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// waitFor polls cond until true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func getStatus(t *testing.T, srv *httptest.Server) StatusResponse {
	t.Helper()
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200", resp.StatusCode)
	}
	var st StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	return st
}

func TestHandleWork_AcceptsAndRunsInBackground(t *testing.T) {
	release := make(chan struct{})
	started := make(chan WorkRequest, 1)
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		started <- req
		<-release
		return nil
	})

	body, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_1", LastHeartbeat: "tok"})
	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /work status = %d, want 200", resp.StatusCode)
	}
	var wr WorkResponse
	_ = json.NewDecoder(resp.Body).Decode(&wr)
	if wr.Status != "accepted" {
		t.Errorf("status = %q, want accepted", wr.Status)
	}

	got := <-started
	if got.SessionID != "sesn_1" || got.WorkID != "work_1" || got.LastHeartbeat != "tok" {
		t.Errorf("handleItem received %+v", got)
	}
	waitFor(t, "running status", func() bool { return getStatus(t, srv).Running })
	st := getStatus(t, srv)
	if st.SessionID != "sesn_1" || st.WorkID != "work_1" {
		t.Errorf("status IDs = %q/%q", st.SessionID, st.WorkID)
	}

	close(release)
	waitFor(t, "work finished", func() bool { return !getStatus(t, srv).Running })
}

// TestHandleWork_DuplicateWorkIDRunsOnce covers the controller retry path:
// a repeated POST /work for the work item already in flight must be answered
// accepted without starting a second execution.
func TestHandleWork_DuplicateWorkIDRunsOnce(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		calls.Add(1)
		<-release
		return nil
	})

	body, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_1", LastHeartbeat: "tok"})
	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()
	waitFor(t, "first work in flight", func() bool { return getStatus(t, srv).Running })

	dupResp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("duplicate POST /work: %v", err)
	}
	defer dupResp.Body.Close()
	if dupResp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200", dupResp.StatusCode)
	}
	var wr WorkResponse
	_ = json.NewDecoder(dupResp.Body).Decode(&wr)
	if wr.Status != "accepted" {
		t.Errorf("duplicate status = %q, want accepted", wr.Status)
	}

	close(release)
	waitFor(t, "work finished", func() bool { return !getStatus(t, srv).Running })
	if got := calls.Load(); got != 1 {
		t.Errorf("handleItem ran %d times, want 1", got)
	}
}

// TestHandleWork_RetryAfterCompletionDoesNotRerun covers the case where the
// controller's status polls failed while the work actually finished: the
// retried POST /work must be answered accepted without a second execution.
func TestHandleWork_RetryAfterCompletionDoesNotRerun(t *testing.T) {
	var calls atomic.Int32
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		calls.Add(1)
		return nil
	})

	body, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_1", LastHeartbeat: "tok"})
	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()
	waitFor(t, "first run finished", func() bool { return !getStatus(t, srv).Running })

	resp2, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("retry POST /work: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", resp2.StatusCode)
	}
	var wr WorkResponse
	_ = json.NewDecoder(resp2.Body).Decode(&wr)
	if wr.Status != "accepted" {
		t.Errorf("retry status = %q, want accepted", wr.Status)
	}

	// Give a wrongful second execution a chance to show up.
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("handleItem ran %d times, want 1 (retry after completion must not rerun)", got)
	}
}

// TestHandleWork_FailedRunIsNotCached verifies a failed execution is not
// recorded as completed: when CMA re-delivers the same work item, the
// runner must execute it again instead of answering accepted from the
// completed cache.
func TestHandleWork_FailedRunIsNotCached(t *testing.T) {
	var calls atomic.Int32
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		if calls.Add(1) == 1 {
			return errors.New("transient tool failure")
		}
		return nil
	})

	body, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_1", LastHeartbeat: "tok"})
	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()
	waitFor(t, "failed run finished", func() bool { return !getStatus(t, srv).Running && calls.Load() == 1 })

	resp2, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("re-delivery POST /work: %v", err)
	}
	resp2.Body.Close()
	waitFor(t, "re-delivered run executed", func() bool { return calls.Load() == 2 })
}

// TestHandleWork_DifferentWorkWhileBusyIsRejected covers the case where the
// runner receives a different work item while one is in flight: it must
// reject it (so the controller retries later) instead of queueing it, which
// would let /status report idle while the queued run has not started.
func TestHandleWork_DifferentWorkWhileBusyIsRejected(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		calls.Add(1)
		<-release
		return nil
	})

	bodyA, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_A"})
	respA, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(bodyA))
	if err != nil {
		t.Fatalf("POST /work A: %v", err)
	}
	respA.Body.Close()
	waitFor(t, "A in flight", func() bool { return getStatus(t, srv).Running })

	bodyB, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_B"})
	respB, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(bodyB))
	if err != nil {
		t.Fatalf("POST /work B: %v", err)
	}
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("B status = %d, want 409 while A is in flight", respB.StatusCode)
	}

	close(release)
	waitFor(t, "A finished", func() bool { return !getStatus(t, srv).Running })
	if got := calls.Load(); got != 1 {
		t.Errorf("handleItem ran %d times, want 1 (B must not be queued)", got)
	}
}

func TestHandleWork_MissingSessionID(t *testing.T) {
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		t.Error("handleItem must not run without session_id")
		return nil
	})

	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleWork_MethodNotAllowed(t *testing.T) {
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error { return nil })
	resp, err := http.Get(srv.URL + "/work")
	if err != nil {
		t.Fatalf("GET /work: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestIdle_ReportsIdleDuration(t *testing.T) {
	release := make(chan struct{})
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error {
		<-release
		return nil
	})

	body, _ := json.Marshal(WorkRequest{SessionID: "sesn_1", WorkID: "work_1"})
	resp, err := http.Post(srv.URL+"/work", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()
	close(release)
	waitFor(t, "work finished", func() bool { return !getStatus(t, srv).Running })

	resp2, err := http.Get(srv.URL + "/idle")
	if err != nil {
		t.Fatalf("GET /idle: %v", err)
	}
	defer resp2.Body.Close()
	var idle IdleStatus
	_ = json.NewDecoder(resp2.Body).Decode(&idle)
	d, err := time.ParseDuration(idle.IdleDuration)
	if err != nil {
		t.Fatalf("idle_duration %q not a Go duration: %v", idle.IdleDuration, err)
	}
	if d < 0 {
		t.Errorf("idle_duration = %v, want >= 0", d)
	}
	if _, err := time.Parse(time.RFC3339, idle.LastActiveAt); err != nil {
		t.Errorf("last_active_at %q not RFC3339: %v", idle.LastActiveAt, err)
	}
}

func TestHealthz(t *testing.T) {
	_, srv := newTestRunner(t, func(ctx context.Context, req WorkRequest) error { return nil })
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}
}
