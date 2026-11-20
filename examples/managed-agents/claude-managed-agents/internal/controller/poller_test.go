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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/go-logr/logr"
)

// fakeCMA is a minimal stand-in for the Anthropic CMA work API. The SDK sends
// request parameters as query values, so handlers record the raw query for
// assertions.
type fakeCMA struct {
	mu       sync.Mutex
	requests []string // "METHOD path" in order
	queries  []string // raw query strings, same order

	pollCode  int // status the poll endpoint returns
	pollBody  string
	ackCode   int
	hbCode    int
	hbBody    string
	stopCode  int
	pollCount int
}

const workJSON = `{"id":"work_1","environment_id":"env_1","state":"queued","secret":"s3cret","data":{"id":"sesn_1","type":"session"},"type":"work"}`

const heartbeatOKJSON = `{"last_heartbeat":"tok-1","lease_extended":true,"state":"active","ttl_seconds":120,"type":"work_heartbeat"}`

// noWorkJSON is the error body the API returns with a 404 when the
// environment has no claimable work.
const noWorkJSON = `{"type":"error","error":{"type":"not_found_error","message":"no work available"}}`

func (f *fakeCMA) record(r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.queries = append(f.queries, r.URL.RawQuery)
}

func (f *fakeCMA) seen(suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if strings.HasSuffix(req, suffix) {
			return true
		}
	}
	return false
}

// queryOf returns the raw query of the first request whose path ends with
// suffix, or "" when there is none.
func (f *fakeCMA) queryOf(suffix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, req := range f.requests {
		if strings.HasSuffix(req, suffix) {
			return f.queries[i]
		}
	}
	return ""
}

func (f *fakeCMA) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		// The SDK refuses to decode responses without a JSON content type.
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/work/poll"):
			f.mu.Lock()
			f.pollCount++
			n := f.pollCount
			code, body := f.pollCode, f.pollBody
			f.mu.Unlock()
			if n > 1 && code == http.StatusOK {
				// Serve the item once; later polls report no work so the
				// loop under test goes idle instead of re-delivering.
				code, body = http.StatusNotFound, noWorkJSON
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		case strings.HasSuffix(r.URL.Path, "/ack"):
			w.WriteHeader(f.ackCode)
			if f.ackCode == http.StatusOK {
				_, _ = io.WriteString(w, workJSON)
			}
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			w.WriteHeader(f.hbCode)
			if f.hbCode == http.StatusOK {
				_, _ = io.WriteString(w, f.hbBody)
			}
		case strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(f.stopCode)
			if f.stopCode == http.StatusOK {
				_, _ = io.WriteString(w, workJSON)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTestPoller(t *testing.T, f *fakeCMA) *Poller {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithoutEnvironmentDefaults(),
	)
	p, err := NewPoller(PollerConfig{
		Client:            &client,
		EnvironmentID:     "env_1",
		HandoffTTLSeconds: 120,
		PollInterval:      10 * time.Millisecond,
		Logger:            logr.Discard(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

func TestPollOnce_NoWork_IsNoWork(t *testing.T) {
	f := &fakeCMA{pollCode: http.StatusNotFound, pollBody: noWorkJSON, ackCode: 200, hbCode: 200, hbBody: heartbeatOKJSON, stopCode: 200}
	p := newTestPoller(t, f)
	_, err := p.PollOnce(context.Background())
	if err != ErrNoWork {
		t.Errorf("err = %v, want ErrNoWork", err)
	}
}

func TestLease_AckThenFirstHeartbeat(t *testing.T) {
	f := &fakeCMA{pollCode: http.StatusOK, pollBody: workJSON, ackCode: http.StatusOK, hbCode: http.StatusOK, hbBody: heartbeatOKJSON, stopCode: http.StatusOK}
	p := newTestPoller(t, f)

	item, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if item.ID != "work_1" || item.Data.ID != "sesn_1" {
		t.Fatalf("polled item = %+v", item)
	}

	tok, err := p.Lease(context.Background(), item)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if tok != "tok-1" {
		t.Errorf("token = %q, want tok-1", tok)
	}
	if !f.seen("/work/work_1/ack") {
		t.Error("ack endpoint not called")
	}
	if !f.seen("/work/work_1/heartbeat") {
		t.Error("heartbeat endpoint not called")
	}
	q := f.queryOf("/heartbeat")
	if !strings.Contains(q, "expected_last_heartbeat=NO_HEARTBEAT") {
		t.Errorf("first heartbeat did not use the NO_HEARTBEAT sentinel: %s", q)
	}
	if !strings.Contains(q, "desired_ttl_seconds=120") {
		t.Errorf("first heartbeat did not request the hand-off TTL: %s", q)
	}
	if f.seen("/stop") {
		t.Error("stop called on the happy path")
	}
}

func TestLease_AckFails_NoStop(t *testing.T) {
	f := &fakeCMA{pollCode: http.StatusOK, pollBody: workJSON, ackCode: http.StatusInternalServerError, hbCode: http.StatusOK, hbBody: heartbeatOKJSON, stopCode: http.StatusOK}
	p := newTestPoller(t, f)

	item, _ := p.PollOnce(context.Background())
	if _, err := p.Lease(context.Background(), item); err == nil {
		t.Fatal("Lease succeeded despite ack failure")
	}
	if f.seen("/heartbeat") {
		t.Error("heartbeat must not follow a failed ack")
	}
	if f.seen("/stop") {
		t.Error("an unacked item is left for the API to re-deliver; no stop")
	}
}

func TestLease_HeartbeatFails_ForceStops(t *testing.T) {
	f := &fakeCMA{pollCode: http.StatusOK, pollBody: workJSON, ackCode: http.StatusOK, hbCode: http.StatusInternalServerError, stopCode: http.StatusOK}
	p := newTestPoller(t, f)

	item, _ := p.PollOnce(context.Background())
	if _, err := p.Lease(context.Background(), item); err == nil {
		t.Fatal("Lease succeeded despite heartbeat failure")
	}
	if !f.seen("/work/work_1/stop") {
		t.Error("acked-but-unleased item must be force-stopped")
	}
}

func TestPoll_DispatchesClaimsUntilCancelled(t *testing.T) {
	f := &fakeCMA{pollCode: http.StatusOK, pollBody: workJSON, ackCode: http.StatusOK, hbCode: http.StatusOK, hbBody: heartbeatOKJSON, stopCode: http.StatusOK}
	p := newTestPoller(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	claims := make(chan Claim, 4)
	done := make(chan error, 1)
	go func() {
		done <- p.Poll(ctx, func(ctx context.Context, c Claim) error {
			claims <- c
			return nil
		})
	}()

	select {
	case c := <-claims:
		if c.Work.ID != "work_1" || c.LastHeartbeat != "tok-1" {
			t.Errorf("claim = %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no claim dispatched")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Poll returned nil on cancel; want ctx error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after cancel")
	}
}
