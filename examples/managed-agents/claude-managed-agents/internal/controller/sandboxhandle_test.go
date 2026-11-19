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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
)

// newHandleForTest builds a sandboxHandle talking to the test server, with
// the SDK parts left nil (PostWork does not touch them).
func newHandleForTest(srv *httptest.Server) *sandboxHandle {
	return &sandboxHandle{
		f:         &HandleFactory{Logger: logr.Discard()},
		sessionID: "sesn_1",
		http:      srv.Client(),
		log:       logr.Discard(),
		baseURL:   srv.URL,
	}
}

// TestPostWork_TruncatedAcceptanceIsUncertain covers a runner that accepts
// the work but whose 200 response body is interrupted: delivery must be
// classified uncertain so the runner-active window opens.
func TestPostWork_TruncatedAcceptanceIsUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100") // promise more than delivered
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stat`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack-free abrupt close: panic-free way is to use CloseNotify...
		// simplest is to close the underlying connection via hijack.
	}))
	defer srv.Close()

	h := newHandleForTest(srv)
	err := h.PostWork(context.Background(), WorkEvent{SessionID: "sesn_1", WorkID: "work_1"})
	if err == nil {
		t.Fatal("PostWork succeeded despite a truncated acceptance body")
	}
	if !IsUncertainPost(err) {
		t.Errorf("truncated 200 classified as certain rejection: %v", err)
	}
}

// TestPostWork_MalformedAcceptanceIsUncertain covers a 200 whose body is not
// valid JSON: no definite answer, so delivery must be uncertain.
func TestPostWork_MalformedAcceptanceIsUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `not json`)
	}))
	defer srv.Close()

	h := newHandleForTest(srv)
	err := h.PostWork(context.Background(), WorkEvent{SessionID: "sesn_1", WorkID: "work_1"})
	if err == nil {
		t.Fatal("PostWork succeeded despite a malformed acceptance body")
	}
	if !IsUncertainPost(err) {
		t.Errorf("malformed 200 classified as certain rejection: %v", err)
	}
}

// TestPostWork_BusyRejectionIsCertain covers the runner's definite 409: the
// work is known not to run, so no window should open.
func TestPostWork_BusyRejectionIsCertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"status":"busy","error":"another work item is in flight"}`)
	}))
	defer srv.Close()

	h := newHandleForTest(srv)
	err := h.PostWork(context.Background(), WorkEvent{SessionID: "sesn_1", WorkID: "work_1"})
	if err == nil {
		t.Fatal("PostWork succeeded despite 409")
	}
	if IsUncertainPost(err) {
		t.Errorf("409 misclassified as uncertain: %v", err)
	}
}
