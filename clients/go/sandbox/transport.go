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

package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/propagation"
)

const (
	// maxAttempts is the total number of HTTP attempts (1 initial + 5 retries).
	maxAttempts = 6
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 8 * time.Second
)

func generateRequestID() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], rand.Uint64())
	return hex.EncodeToString(b[:])
}

var retryableStatusCodes = map[int]bool{
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// cancelOnClose wraps an io.ReadCloser to run a cleanup function (typically
// cancelling per-attempt and request-level contexts) when the caller closes
// the response body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// doRequest sends an HTTP request to the sandbox router with required headers
// and retry logic for transient failures. The body must implement io.Seeker
// for retries to work; non-seekable bodies are sent at most once.
//
// Connection state (URL, claim name) is re-read on each retry attempt so the
// client can detect port-forward death or pick up a concurrent lifecycle change.
func (c *SandboxClient) doRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	// When the caller's context has no deadline, apply RequestTimeout as a
	// ceiling for all retry attempts. On success the cancel is transferred
	// to cancelOnClose so the timeout stays alive while the caller reads the
	// response body; the defer only fires on error paths.
	var requestCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, requestCancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
	}
	defer func() {
		if requestCancel != nil {
			requestCancel()
		}
	}()

	reqID := generateRequestID()

	var lastErr error
	var lastURL string
	for attempt := range maxAttempts {
		// Re-snapshot connection state on each attempt to detect
		// port-forward death or pick up concurrent lifecycle changes.
		c.mu.Lock()
		if c.baseURL == "" {
			pfErr := c.lastError
			c.mu.Unlock()
			if pfErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrNotReady, pfErr)
			}
			return nil, ErrNotReady
		}
		reqURL := strings.TrimRight(c.baseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
		lastURL = reqURL
		claimName := c.claimName
		namespace := c.opts.Namespace
		port := c.opts.ServerPort
		c.mu.Unlock()

		var bodyReader io.Reader
		if body != nil {
			if seeker, ok := body.(io.Seeker); ok {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return nil, fmt.Errorf("sandbox: failed to reset request body: %w", err)
				}
				bodyReader = body
			} else if attempt == 0 {
				bodyReader = body
			} else {
				return nil, fmt.Errorf("sandbox: cannot retry after %w with non-seekable body", lastErr)
			}
		}

		// Per-attempt deadline bounds time to first byte. Use cancel +
		// AfterFunc so the timer can be stopped on success, letting the
		// request-level timeout govern body reads instead.
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		attemptTimer := time.AfterFunc(c.opts.PerAttemptTimeout, attemptCancel)

		req, err := http.NewRequestWithContext(attemptCtx, method, reqURL, bodyReader)
		if err != nil {
			attemptTimer.Stop()
			attemptCancel()
			return nil, fmt.Errorf("sandbox: failed to create request: %w", err)
		}

		req.Header.Set(headerSandboxID, claimName)
		req.Header.Set(headerSandboxNamespace, namespace)
		req.Header.Set(headerSandboxPort, strconv.Itoa(port))
		req.Header.Set(headerRequestID, reqID)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		propagation.TraceContext{}.Inject(attemptCtx, propagation.HeaderCarrier(req.Header))

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			attemptTimer.Stop()
			attemptCancel()
			lastErr = doErr
			if ctx.Err() != nil {
				return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
			}
			if attempt < maxAttempts-1 {
				c.log.V(1).Info("request failed, retrying", "attempt", attempt+1, "maxAttempts", maxAttempts, "method", method, "url", reqURL, "reqID", reqID, "error", doErr)
				c.sleepWithContext(ctx, c.backoff(attempt))
				if ctx.Err() != nil {
					return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
				}
			}
			continue
		}

		if retryableStatusCodes[resp.StatusCode] {
			if attempt >= maxAttempts-1 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				attemptTimer.Stop()
				attemptCancel()
				httpErr := &HTTPError{StatusCode: resp.StatusCode, Body: string(errBody), Operation: method + " " + endpoint}
				c.log.Error(httpErr, "retries exhausted", "method", method, "url", reqURL, "attempts", maxAttempts, "lastStatus", resp.StatusCode, "reqID", reqID, "claim", claimName, "namespace", namespace)
				return nil, fmt.Errorf("%w: %s failed after %d attempts (url=%s reqID=%s): %w",
					ErrRetriesExhausted, method, maxAttempts, reqURL, reqID, httpErr)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			attemptTimer.Stop()
			attemptCancel()
			lastErr = fmt.Errorf("sandbox: server returned %d", resp.StatusCode)
			c.log.V(1).Info("retryable status, retrying", "attempt", attempt+1, "maxAttempts", maxAttempts, "method", method, "url", reqURL, "status", resp.StatusCode, "reqID", reqID)
			c.sleepWithContext(ctx, c.backoff(attempt))
			if ctx.Err() != nil {
				return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
			}
			continue
		}

		// Success — stop the per-attempt timer so body reads are
		// governed by the request-level timeout, not per-attempt.
		attemptTimer.Stop()
		rc := requestCancel
		requestCancel = nil // prevent the defer from cancelling
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() {
			attemptCancel()
			if rc != nil {
				rc()
			}
		}}
		return resp, nil
	}

	c.mu.Lock()
	finalClaim := c.claimName
	finalNS := c.opts.Namespace
	c.mu.Unlock()
	c.log.Error(lastErr, "retries exhausted", "method", method, "url", lastURL, "attempts", maxAttempts, "reqID", reqID, "claim", finalClaim, "namespace", finalNS)
	return nil, fmt.Errorf("%w: %s failed after %d attempts (url=%s reqID=%s): %w", ErrRetriesExhausted, method, maxAttempts, lastURL, reqID, lastErr)
}

// backoff returns the delay before the next retry attempt. The first retry
// is immediate; subsequent retries use exponential backoff from baseBackoff
// to maxBackoff with ±25% jitter.
func (c *SandboxClient) backoff(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}
	d := time.Duration(float64(baseBackoff) * math.Pow(2, float64(attempt-1)))
	if d > maxBackoff {
		d = maxBackoff
	}
	c.mu.Lock()
	scale := c.backoffScale
	c.mu.Unlock()
	if scale > 0 {
		d = time.Duration(float64(d) * scale)
	}
	if d <= 0 {
		return time.Millisecond
	}
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	jitter := time.Duration(rand.Int64N(half)) - d/4
	return d + jitter
}

func (c *SandboxClient) sleepWithContext(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
