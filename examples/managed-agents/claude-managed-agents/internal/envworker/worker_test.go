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

package envworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// newTestClient returns an Anthropic client pointed at the test server.
func newTestClient(url string) anthropic.Client {
	return anthropic.NewClient(
		option.WithBaseURL(url),
		option.WithAPIKey("test-key"),
		option.WithoutEnvironmentDefaults(),
	)
}

func newTestWork() *anthropic.BetaSelfHostedWork {
	return &anthropic.BetaSelfHostedWork{
		ID:            "work_1",
		EnvironmentID: "env_1",
		Data:          anthropic.BetaSessionWorkData{ID: "sesn_1"},
	}
}

// TestRunHeartbeat_QuotesHandoffToken_On412MarksLeaseLost verifies that the
// first heartbeat quotes the handed-off token and that a 412 answer marks
// the lease lost without further beats.
func TestRunHeartbeat_QuotesHandoffToken_On412MarksLeaseLost(t *testing.T) {
	// The SDK sends heartbeat params as query parameters, not a JSON body.
	var gotExpectedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/work/work_1/heartbeat") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotExpectedToken = r.URL.Query().Get("expected_last_heartbeat")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"precondition failed"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancels := 0
	wrappedCancel := func() { cancels++; cancel() }
	firstBeat := make(chan bool, 1)

	lost := runHeartbeat(ctx, wrappedCancel, newTestClient(srv.URL), newTestWork(), nil, "tok-handoff", slog.Default(), firstBeat)

	if ok := <-firstBeat; ok {
		t.Error("firstBeat = true, want false on 412")
	}
	if !lost {
		t.Errorf("lost = false, want true on 412")
	}
	if cancels != 1 {
		t.Errorf("cancel called %d times, want 1", cancels)
	}
	if gotExpectedToken != "tok-handoff" {
		t.Errorf("first heartbeat expected_last_heartbeat = %q, want the hand-off token %q", gotExpectedToken, "tok-handoff")
	}
}

// TestRunHeartbeat_StoppedState_ExitsWithoutLeaseLoss verifies a heartbeat
// response reporting the work as stopped ends the loop cleanly.
func TestRunHeartbeat_StoppedState_ExitsWithoutLeaseLoss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"last_heartbeat":"tok-2","lease_extended":true,"state":"stopped","ttl_seconds":30,"type":"work_heartbeat"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstBeat := make(chan bool, 1)

	lost := runHeartbeat(ctx, cancel, newTestClient(srv.URL), newTestWork(), nil, "", slog.Default(), firstBeat)

	// firstBeat means "proceed with tool execution". A work item already in
	// the stopped state must not run tools even though the HTTP call itself
	// succeeded, so firstBeat is false — but the lease was not lost to
	// another claimant.
	if ok := <-firstBeat; ok {
		t.Error("firstBeat = true, want false when the API reports the item stopped")
	}
	if lost {
		t.Errorf("lost = true, want false for a clean stop")
	}
}

// TestRunHeartbeat_SuccessThenContextCancel verifies the steady-state loop
// extends the lease and exits on context cancellation without loss.
func TestRunHeartbeat_SuccessThenContextCancel(t *testing.T) {
	beats := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"last_heartbeat": "tok-3", "lease_extended": true,
			"state": "active", "ttl_seconds": 30, "type": "work_heartbeat",
		}
		_ = json.NewEncoder(w).Encode(resp)
		select {
		case beats <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	firstBeat := make(chan bool, 1)
	done := make(chan bool, 1)
	go func() {
		done <- runHeartbeat(ctx, cancel, newTestClient(srv.URL), newTestWork(), nil, "", slog.Default(), firstBeat)
	}()
	if ok := <-firstBeat; !ok {
		t.Error("firstBeat = false, want true")
	}

	select {
	case <-beats:
	case <-time.After(5 * time.Second):
		t.Fatal("first heartbeat never arrived")
	}
	cancel()

	select {
	case lost := <-done:
		if lost {
			t.Errorf("lost = true, want false after context cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHeartbeat did not return after cancel")
	}
}

// TestRunHeartbeat_BeatBoundedByRemainingLease verifies a beat cannot stay
// pending past the lease lifetime: with a 3s TTL the second request must
// actually start, and its hanging response must be cut off by the per-beat
// deadline (remaining lease), ending the loop with the lease marked lost.
func TestRunHeartbeat_BeatBoundedByRemainingLease(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"last_heartbeat":"tok-9","lease_extended":true,"state":"active","ttl_seconds":3,"type":"work_heartbeat"}`))
			return
		}
		// Hang the second beat: the per-beat deadline (bounded by the
		// remaining lease) must cut it off instead of waiting forever.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstBeat := make(chan bool, 1)
	done := make(chan bool, 1)
	go func() {
		done <- runHeartbeat(ctx, cancel, newTestClient(srv.URL), newTestWork(), nil, "", slog.Default(), firstBeat)
	}()

	if ok := <-firstBeat; !ok {
		t.Fatal("firstBeat = false, want true")
	}
	select {
	case lost := <-done:
		if !lost {
			t.Error("lost = false, want true when a beat cannot complete within the lease lifetime")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runHeartbeat did not return within the lease bound")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests < 2 {
		t.Errorf("second heartbeat never started (requests=%d); the wait must be capped by the remaining lease", requests)
	}
}

// TestRunHeartbeat_SlowResponseUsesRenewalTime verifies the lease deadline
// is computed from when the renewal request was sent, not when the response
// arrived: with a 700ms-slow response and a 2s TTL, the remaining lease at
// the next scheduled beat is below the floor, so the loop must stop with the
// lease marked lost instead of issuing a second (too-late) heartbeat.
func TestRunHeartbeat_SlowResponseUsesRenewalTime(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		time.Sleep(700 * time.Millisecond) // server renews now; response arrives late
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"last_heartbeat":"tok-s","lease_extended":true,"state":"active","ttl_seconds":2,"type":"work_heartbeat"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstBeat := make(chan bool, 1)
	done := make(chan bool, 1)
	go func() {
		done <- runHeartbeat(ctx, cancel, newTestClient(srv.URL), newTestWork(), nil, "", slog.Default(), firstBeat)
	}()

	if ok := <-firstBeat; !ok {
		t.Fatal("firstBeat = false, want true")
	}
	select {
	case lost := <-done:
		if !lost {
			t.Error("lost = false; the deadline must be computed from the renewal time, not the response arrival")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHeartbeat did not stop at the conservative lease deadline")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("requests = %d, want 1: a second heartbeat would start after the server-side lease expired", requests)
	}
}

// TestRunHeartbeat_TransientFirstError_DoesNotConfirm verifies that a
// transient failure of the first heartbeat does not confirm the lease:
// firstBeat must stay unsent until a heartbeat actually succeeds, so
// HandleItem cannot start tools on an unconfirmed lease.
func TestRunHeartbeat_TransientFirstError_DoesNotConfirm(t *testing.T) {
	arrived := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstBeat := make(chan bool, 1)
	done := make(chan bool, 1)
	go func() {
		done <- runHeartbeat(ctx, cancel, newTestClient(srv.URL), newTestWork(), nil, "", slog.Default(), firstBeat)
	}()

	<-arrived // first beat hit the server and failed transiently
	select {
	case ok := <-firstBeat:
		t.Fatalf("firstBeat sent (%v) despite no successful heartbeat", ok)
	case <-time.After(300 * time.Millisecond):
		// correct: not confirmed
	}

	cancel()
	select {
	case ok := <-firstBeat:
		if ok {
			t.Error("firstBeat = true after cancel without a successful beat")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("firstBeat never closed out after cancel; HandleItem would hang")
	}
	select {
	case lost := <-done:
		if lost {
			t.Error("lost = true, want false for transient errors")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHeartbeat did not return after cancel")
	}
}

// TestErrAttrs_OmitsURLAndToken verifies heartbeat errors are logged as
// status/request-id only: the SDK error string embeds the request URL, whose
// query carries the lease token.
func TestErrAttrs_OmitsURLAndToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"http://api.example/v1/environments/env_1/work/work_1/heartbeat?expected_last_heartbeat=tok-secret", nil)
	apiErr := &anthropic.Error{StatusCode: 412, RequestID: "req_1", Request: req}

	attrs := errAttrs(apiErr)
	var flat string
	for _, a := range attrs {
		flat += fmt.Sprint(a) + " "
	}
	if strings.Contains(flat, "tok-secret") || strings.Contains(flat, "http") {
		t.Errorf("errAttrs leaked URL/token material: %s", flat)
	}
	if !strings.Contains(flat, "412") || !strings.Contains(flat, "req_1") {
		t.Errorf("errAttrs missing status/request_id: %s", flat)
	}
}

// TestHandleItem_LeaseLost412_SkipsTools verifies no tool execution or
// session-events call happens when the first heartbeat reports the lease was
// reclaimed by another worker.
func TestHandleItem_LeaseLost412_SkipsTools(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"precondition failed"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	w := NewEnvironmentWorker(newTestClient(srv.URL), EnvironmentWorkerOptions{
		Workdir: t.TempDir(),
	})
	err := w.HandleItem(context.Background(), HandleItemOptions{
		WorkID:                "work_1",
		EnvironmentID:         "env_1",
		SessionID:             "sesn_1",
		EnvironmentKey:        "env-key",
		ExpectedLastHeartbeat: "tok-stale",
	})
	if err != nil {
		t.Errorf("HandleItem = %v, want nil when the lease was lost before any tool ran", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.Contains(p, "/sessions/") {
			t.Errorf("session endpoint %q called despite losing the lease", p)
		}
	}
}

// TestIsFatal4xx covers the transient/permanent classification.
func TestIsFatal4xx(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{400, true}, {408, false}, {409, false}, {412, true}, {429, false}, {500, false}} {
		apiErr := &anthropic.Error{StatusCode: tc.code}
		if got := isFatal4xx(apiErr); got != tc.want {
			t.Errorf("isFatal4xx(status %d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
