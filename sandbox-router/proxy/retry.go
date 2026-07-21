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
	"errors"
	"net"
	"net/http"
	"time"
)

// retryTransport wraps an http.RoundTripper and retries on dial-class errors.
// Retrying on dial failures is safe for any request — including POST/PUT with
// non-replayable bodies — because Go's http.Transport does not touch the
// request body until after a successful Dial. After-dial failures
// (response timeouts, mid-stream EOF, etc.) bypass the retry path so we never
// risk re-sending a partially-consumed body.
//
// Retry budget is bounded both by maxAttempts and by the per-request context
// deadline (set by the caller via context.WithTimeout). Sleeps are interrupted
// when the context fires.
type retryTransport struct {
	base         http.RoundTripper
	maxAttempts  int           // total attempts including the first; 1 = no retries
	initialDelay time.Duration // delay before the first retry
	maxDelay     time.Duration // upper bound on the per-iteration delay
	onRetry      func(req *http.Request, err error, attempt int)
}

// newRetryTransport wraps base. attempts <= 1 disables retries entirely.
func newRetryTransport(base http.RoundTripper, attempts int, initialDelay, maxDelay time.Duration, onRetry func(*http.Request, error, int)) *retryTransport {
	if attempts < 1 {
		attempts = 1
	}
	if initialDelay <= 0 {
		initialDelay = 200 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 800 * time.Millisecond
	}
	return &retryTransport{
		base:         base,
		maxAttempts:  attempts,
		initialDelay: initialDelay,
		maxDelay:     maxDelay,
		onRetry:      onRetry,
	}
}

// RoundTrip implements http.RoundTripper.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	delay := t.initialDelay
	var lastErr error

	timer := time.NewTimer(delay)
	defer timer.Stop()

	for attempt := 1; attempt <= t.maxAttempts; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetriableDialError(err) || attempt == t.maxAttempts {
			return nil, err
		}
		if t.onRetry != nil {
			t.onRetry(req, err, attempt)
		}

		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timer.Reset(delay)

		select {
		case <-timer.C:
		case <-req.Context().Done():
			// Surface the original dial error rather than the context error so
			// the proxy ErrorHandler reports the actual upstream failure.
			return nil, lastErr
		}

		delay *= 2
		if delay > t.maxDelay {
			delay = t.maxDelay
		}
	}
	return nil, lastErr
}

// isRetriableDialError reports whether err indicates a dial-time failure that
// is safe to retry: either DNS resolution failed (the sandbox Service may not
// be registered yet) or the TCP connect was refused / timed out (the sandbox
// Pod may not be listening yet).
//
// Errors from later in the request lifecycle (e.g. response read timeouts)
// are intentionally NOT retried because the request body may already have
// been streamed to the upstream and replaying it would duplicate side effects.
func isRetriableDialError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// "dial" covers connection refused, no route, ECONNRESET on connect,
		// and dial deadline. Any other Op (e.g. "read", "write") happens
		// after the body has been touched and is intentionally not retried.
		if opErr.Op == "dial" {
			return true
		}
	}
	return false
}
