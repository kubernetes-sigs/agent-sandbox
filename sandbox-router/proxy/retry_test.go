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
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// dialErr builds an error shaped like what *net.OpError{Op:"dial"} produces;
// we wrap a string error to mimic the inner error without binding to a syscall.
func dialErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func dnsErr() error {
	return &net.DNSError{Err: "no such host", Name: "x.y.z"}
}

func TestIsRetriableDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial OpError", dialErr(), true},
		{"DNS error", dnsErr(), true},
		{"wrapped dial", &net.OpError{Op: "dial", Err: errors.New("x")}, true},
		{"read OpError (not retried)", &net.OpError{Op: "read", Err: errors.New("x")}, false},
		{"write OpError (not retried)", &net.OpError{Op: "write", Err: errors.New("x")}, false},
		{"plain error", errors.New("boom"), false},
		{"context deadline", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetriableDialError(tc.err); got != tc.want {
				t.Errorf("isRetriableDialError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func newRequest(ctx context.Context, t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRetryTransport_SucceedsOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	tr := newRetryTransport(base, 3, 1*time.Millisecond, 10*time.Millisecond, nil)
	resp, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 call, got %d", got)
	}
}

func TestRetryTransport_RetriesOnDialFailureThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n < 3 {
			return nil, dialErr()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	var retries atomic.Int32
	onRetry := func(*http.Request, error, int) { retries.Add(1) }
	tr := newRetryTransport(base, 5, 1*time.Millisecond, 10*time.Millisecond, onRetry)
	resp, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
	if got := retries.Load(); got != 2 {
		t.Errorf("expected 2 onRetry callbacks, got %d", got)
	}
}

func TestRetryTransport_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	want := dialErr()
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, want
	})
	tr := newRetryTransport(base, 3, 1*time.Millisecond, 10*time.Millisecond, nil)
	_, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !errors.Is(err, want) {
		t.Errorf("expected original dial error, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", got)
	}
}

func TestRetryTransport_DoesNotRetryNonDialErrors(t *testing.T) {
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("read tcp: connection reset by peer")
	})
	tr := newRetryTransport(base, 5, 1*time.Millisecond, 10*time.Millisecond, nil)
	_, err := tr.RoundTrip(newRequest(t.Context(), t))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("non-dial error should not be retried; got %d attempts", got)
	}
}

func TestRetryTransport_StopsOnContextCancel(t *testing.T) {
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, dialErr()
	})
	// Long backoff so we can cancel mid-wait.
	tr := newRetryTransport(base, 100, 500*time.Millisecond, 2*time.Second, nil)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := tr.RoundTrip(newRequest(ctx, t))
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	// At least one attempt happened; should be far fewer than 100.
	if c := calls.Load(); c < 1 || c > 5 {
		t.Errorf("expected 1-5 attempts before cancel, got %d", c)
	}
}

func TestRetryTransport_AttemptsClampedToOne(t *testing.T) {
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, dialErr()
	})
	tr := newRetryTransport(base, 0, 1*time.Millisecond, 10*time.Millisecond, nil)
	_, _ = tr.RoundTrip(newRequest(t.Context(), t))
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts=0 should clamp to 1 attempt, got %d", got)
	}
}

func TestRetryTransport_BackoffGrowsAndCaps(t *testing.T) {
	// Verify that delay growth + cap behavior is correct: the transport waits
	// between attempts, so a 5-attempt loop with initial=10ms, max=20ms should
	// wait roughly 10+20+20+20 = 70ms. We assert lower-bound to avoid flakiness.
	var calls atomic.Int32
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, dialErr()
	})
	tr := newRetryTransport(base, 5, 10*time.Millisecond, 20*time.Millisecond, nil)
	start := time.Now()
	_, _ = tr.RoundTrip(newRequest(t.Context(), t))
	elapsed := time.Since(start)
	if calls.Load() != 5 {
		t.Errorf("expected 5 attempts, got %d", calls.Load())
	}
	// 10 + 20 + 20 + 20 = 70ms minimum (4 waits between 5 attempts)
	if elapsed < 70*time.Millisecond {
		t.Errorf("expected at least 70ms elapsed, got %s", elapsed)
	}
}
