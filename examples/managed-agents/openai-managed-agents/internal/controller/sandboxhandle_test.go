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

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// newTestHandle builds a sandboxHandle pointed at a test server without any
// cluster involvement; poll intervals are shrunk for fast tests.
func newTestHandle(baseURL string, api AgentAPI) *sandboxHandle {
	return &sandboxHandle{
		f:            &HandleFactory{API: api, Logger: logr.Discard()},
		sessionID:    "s1",
		http:         &http.Client{Timeout: 2 * time.Second},
		log:          logr.Discard(),
		pollInterval: 5 * time.Millisecond,
		turnInterval: 5 * time.Millisecond,
		baseURL:      baseURL,
	}
}

func statusServer(t *testing.T, alive, ready bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case ready:
			w.Write([]byte(`{"alive":true,"ready":true,"exit_code":-1,"session_id":"s1"}`))
		case alive:
			w.Write([]byte(`{"alive":true,"ready":false,"exit_code":-1,"session_id":"s1"}`))
		default:
			w.Write([]byte(`{"alive":false,"ready":false,"exit_code":3,"stderr_tail":"boom","session_id":"s1"}`))
		}
	}))
}

func TestStartExecutorClassification(t *testing.T) {
	ev := testEvent("s1")
	cases := []struct {
		name          string
		handler       http.HandlerFunc
		wantErr       bool
		wantUncertain bool
		wantConflict  bool
	}{
		{
			name:    "started",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"status":"started"}`)) },
		},
		{
			name:    "already running",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"status":"already_running"}`)) },
		},
		{
			name:          "transport-ish 5xx is uncertain",
			handler:       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) },
			wantErr:       true,
			wantUncertain: true,
		},
		{
			name: "409 is a definite binding conflict",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"status":"conflict"}`))
			},
			wantErr:      true,
			wantConflict: true,
		},
		{
			name:          "malformed 200 is uncertain",
			handler:       func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{`)) },
			wantErr:       true,
			wantUncertain: true,
		},
		{
			name: "definite rejection",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(`{"status":"error","error":"exec: not found"}`))
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			h := newTestHandle(srv.URL, nil)
			err := h.StartExecutor(context.Background(), ev, 1)
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr && err == nil {
				t.Fatal("want error")
			}
			if err != nil && IsUncertainPost(err) != tc.wantUncertain {
				t.Fatalf("uncertain = %v, want %v (%v)", IsUncertainPost(err), tc.wantUncertain, err)
			}
			if err != nil && errors.Is(err, errConflictBinding) != tc.wantConflict {
				t.Fatalf("conflict = %v, want %v (%v)", errors.Is(err, errConflictBinding), tc.wantConflict, err)
			}
		})
	}
}

func TestStartExecutorClosedServerUncertain(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	h := newTestHandle(url, nil)
	err := h.StartExecutor(context.Background(), testEvent("s1"), 1)
	if err == nil || !IsUncertainPost(err) {
		t.Fatalf("err = %v, want uncertain", err)
	}
}

func TestWaitConnectedReady(t *testing.T) {
	srv := statusServer(t, true, true)
	defer srv.Close()
	h := newTestHandle(srv.URL, nil)
	if err := h.WaitConnected(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitConnectedExecutorDied(t *testing.T) {
	srv := statusServer(t, false, false)
	defer srv.Close()
	h := newTestHandle(srv.URL, nil)
	err := h.WaitConnected(context.Background(), time.Second)
	if err == nil {
		t.Fatal("want error for dead executor")
	}
	if got := err.Error(); !strings.Contains(got, "exit 3") || !strings.Contains(got, "boom") {
		t.Fatalf("error should surface exit code and stderr tail: %v", err)
	}
}

func TestWaitConnectedCrossCheck(t *testing.T) {
	// Never ready, but the platform no longer requests a connection:
	// the executor is connected even without the marker.
	srv := statusServer(t, true, false)
	defer srv.Close()
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, NeedsConnection: false},
	}}
	h := newTestHandle(srv.URL, api)
	if err := h.WaitConnected(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("cross-check should confirm connection: %v", err)
	}
}

func TestWaitConnectedTimeout(t *testing.T) {
	srv := statusServer(t, true, false)
	defer srv.Close()
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, NeedsConnection: true},
	}}
	h := newTestHandle(srv.URL, api)
	if err := h.WaitConnected(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestWaitTurnSettledWaitsForTerminalState(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, ActiveTurn: true, LatestTurnState: "in_progress"},
	}}
	// Settle the turn shortly after the wait begins.
	time.AfterFunc(30*time.Millisecond, func() {
		api.SetSession(SessionInfo{ID: "s1", Managed: true, ActiveTurn: false, LatestTurnState: "completed"})
	})
	h := newTestHandle("", api)
	state, err := h.WaitTurnSettled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state != "completed" {
		t.Fatalf("state = %q, want completed", state)
	}
}

func TestWaitTurnSettledReturnsFailedTurn(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true, ActiveTurn: false, LatestTurnState: "failed"},
	}}
	h := newTestHandle("", api)
	state, err := h.WaitTurnSettled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("state = %q, want failed (a failed turn is not a bridge error)", state)
	}
}

func TestWaitTurnSettledSessionGone(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{}}
	h := newTestHandle("", api)
	_, err := h.WaitTurnSettled(context.Background())
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestWaitTurnSettledToleratesTransientErrors(t *testing.T) {
	api := &FakeAPI{
		Sessions: map[string]SessionInfo{
			"s1": {ID: "s1", Managed: true, ActiveTurn: false, LatestTurnState: "completed"},
		},
		GetErr: errors.New("500"),
	}
	h := newTestHandle("", api)
	// Fail the first poll or two (5ms interval), then serve — well inside
	// the three-error tolerance.
	time.AfterFunc(8*time.Millisecond, func() {
		api.mu.Lock()
		api.GetErr = nil
		api.mu.Unlock()
	})
	state, err := h.WaitTurnSettled(context.Background())
	if err != nil {
		t.Fatalf("transient errors must be tolerated: %v", err)
	}
	if state != "completed" {
		t.Fatalf("state = %q", state)
	}
}

func TestStopExecutorToleratesTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	h := newTestHandle(url, nil)
	// A lost stop call must not block suspension: the pod termination kills
	// the executor anyway.
	if err := h.StopExecutor(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestClaimNameDeterministicAndValid(t *testing.T) {
	a := ClaimNameForSession("sess_abc123")
	b := ClaimNameForSession("sess_abc123")
	c := ClaimNameForSession("sess_other")
	if a != b {
		t.Fatal("claim name must be deterministic for adoption after restart")
	}
	if a == c {
		t.Fatal("different sessions must not collide")
	}
	if len(a) > 63 {
		t.Fatalf("claim name too long: %d", len(a))
	}
}
