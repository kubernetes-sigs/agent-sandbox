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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func newTestRunner(t *testing.T, mode string) *Runner {
	t.Helper()
	r := NewRunner(RunnerConfig{
		ExecBin:   fakeBin(t),
		ExecArgs:  []string{"exec-server"},
		Env:       []string{"FAKEEXEC_MODE=" + mode},
		StopGrace: time.Second,
		Log:       logr.Discard(),
	})
	// Stops the child even when a test fails early; a no-op when the test
	// already stopped it.
	t.Cleanup(func() { r.stopForTest() })
	return r
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf)))
	return rr
}

func getStatus(t *testing.T, h http.Handler) StatusResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/status code = %d", rr.Code)
	}
	var st StatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decoding status: %v (%s)", err, rr.Body)
	}
	return st
}

func TestExecutorStartIdempotentAndConflict(t *testing.T) {
	r := newTestRunner(t, "ready")
	h := r.Handler()
	a := ExecRequest{SessionID: "s1", EnvironmentID: "env1", RemoteURL: "wss://codex-cloud-environments.chatgpt.com/x", AttemptID: "1"}

	if rr := postJSON(t, h, "/executor", a); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"started"`) {
		t.Fatalf("first start: %d %s", rr.Code, rr.Body)
	}
	// Same binding again → idempotent 200 already_running.
	a2 := a
	a2.AttemptID = "2" // attempt differs; binding (session+env) is what matters
	if rr := postJSON(t, h, "/executor", a2); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `already_running`) {
		t.Fatalf("dup: %d %s", rr.Code, rr.Body)
	}
	// Different session while alive → 409 conflict.
	b := a
	b.SessionID = "s2"
	if rr := postJSON(t, h, "/executor", b); rr.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", rr.Code, rr.Body)
	}

	waitFor(t, 2*time.Second, func() bool { st := getStatus(t, h); return st.Alive && st.Ready })
	st := getStatus(t, h)
	if st.SessionID != "s1" || st.EnvironmentID != "env1" || st.AttemptID != "1" {
		t.Fatalf("bad binding in status: %+v", st)
	}
	// The remote URL is a capability: it must never appear in a response.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	if strings.Contains(rr.Body.String(), "wss://codex-cloud-environments.chatgpt.com/x") {
		t.Fatal("remote_url leaked in /status")
	}
}

func TestExecutorStopLifecycle(t *testing.T) {
	r := newTestRunner(t, "ready")
	h := r.Handler()
	a := ExecRequest{SessionID: "s1", EnvironmentID: "env1", RemoteURL: "wss://codex-cloud-environments.chatgpt.com/x", AttemptID: "1"}
	if rr := postJSON(t, h, "/executor", a); rr.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	waitFor(t, 2*time.Second, func() bool { return getStatus(t, h).Alive })

	if rr := postJSON(t, h, "/executor/stop", nil); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"stopped"`) {
		t.Fatalf("stop: %d %s", rr.Code, rr.Body)
	}
	if st := getStatus(t, h); st.Alive {
		t.Fatal("still alive after stop")
	}
	// Second stop is a no-op, not an error.
	if rr := postJSON(t, h, "/executor/stop", nil); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `not_running`) {
		t.Fatalf("stop again: %d %s", rr.Code, rr.Body)
	}
	// After a stop, a fresh start with different params is allowed.
	b := ExecRequest{SessionID: "s2", EnvironmentID: "env2", RemoteURL: "wss://codex-cloud-environments.chatgpt.com/y", AttemptID: "1"}
	if rr := postJSON(t, h, "/executor", b); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"started"`) {
		t.Fatalf("restart after stop: %d %s", rr.Code, rr.Body)
	}
	waitFor(t, 2*time.Second, func() bool { return getStatus(t, h).SessionID == "s2" })
	_ = r.stopForTest()
}

func TestExecutorCrashVisibleInStatus(t *testing.T) {
	r := newTestRunner(t, "exit3")
	h := r.Handler()
	a := ExecRequest{SessionID: "s1", EnvironmentID: "env1", RemoteURL: "wss://codex-cloud-environments.chatgpt.com/x", AttemptID: "1"}
	if rr := postJSON(t, h, "/executor", a); rr.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	waitFor(t, 3*time.Second, func() bool { st := getStatus(t, h); return !st.Alive && st.ExitCode == 3 })
	st := getStatus(t, h)
	if !strings.Contains(st.StderrTail, "boom") {
		t.Fatalf("stderr tail missing: %q", st.StderrTail)
	}
	// A crashed executor must not auto-restart.
	time.Sleep(150 * time.Millisecond)
	if getStatus(t, h).Alive {
		t.Fatal("auto-restarted after crash")
	}
}

func TestIdleAndHealthz(t *testing.T) {
	r := newTestRunner(t, "ready")
	h := r.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}

	var idle struct {
		IdleSeconds float64 `json:"idle_seconds"`
		LastEventAt string  `json:"last_event_at"`
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/idle", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("idle: %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &idle); err != nil {
		t.Fatal(err)
	}
	if idle.IdleSeconds < 0 {
		t.Fatalf("negative idle: %v", idle.IdleSeconds)
	}

	// An /executor call refreshes idleness.
	a := ExecRequest{SessionID: "s1", EnvironmentID: "env1", RemoteURL: "wss://codex-cloud-environments.chatgpt.com/x", AttemptID: "1"}
	before := idle.IdleSeconds
	if rr := postJSON(t, h, "/executor", a); rr.Code != http.StatusOK {
		t.Fatalf("start: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/idle", nil))
	_ = json.Unmarshal(rr.Body.Bytes(), &idle)
	if idle.IdleSeconds > before+1 {
		t.Fatalf("idle not refreshed: %v > %v", idle.IdleSeconds, before)
	}
	_ = r.stopForTest()
}

func TestExecutorRejectsDisallowedRemoteURL(t *testing.T) {
	r := newTestRunner(t, "ready")
	h := r.Handler()
	for _, u := range []string{
		"",
		"http://codex-cloud-environments.chatgpt.com/x", // wrong scheme
		"wss://evil.example/x",                          // host not allowed
	} {
		rr := postJSON(t, h, "/executor", ExecRequest{
			SessionID: "s1", EnvironmentID: "env1", RemoteURL: u, AttemptID: "1",
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("remote_url %q: code = %d %s, want 400", u, rr.Code, rr.Body)
		}
	}
}

func TestExecutorMalformedBody(t *testing.T) {
	r := newTestRunner(t, "ready")
	h := r.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/executor", strings.NewReader("{")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", rr.Code)
	}
	// Missing session ID is rejected definitely.
	rr = postJSON(t, h, "/executor", ExecRequest{EnvironmentID: "e"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing session: %d", rr.Code)
	}
}
