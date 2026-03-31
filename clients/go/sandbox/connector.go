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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxAttempts = 6
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 8 * time.Second

	// maxDrainBytes caps how much of a response body is read to allow
	// the underlying TCP connection to be reused by net/http's transport.
	maxDrainBytes = 4 << 10
)

var retryableStatusCodes = map[int]bool{
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// connector manages HTTP connectivity to the sandbox-router. It owns the
// HTTP client, retry logic, and delegates URL discovery to a ConnectionStrategy.
type connector struct {
	strategy   ConnectionStrategy
	httpClient *http.Client

	claimName  string
	namespace  string
	serverPort int
	baseURL    string
	lastError  error

	requestTimeout    time.Duration
	perAttemptTimeout time.Duration

	mu           sync.Mutex
	backoffScale float64 // test hook

	ownsTransport bool
	log           logr.Logger
	tracer        trace.Tracer
	svcName string
}

// connectorConfig holds the parameters needed to construct a connector.
type connectorConfig struct {
	Strategy          ConnectionStrategy
	Namespace         string
	ServerPort        int
	RequestTimeout    time.Duration
	PerAttemptTimeout time.Duration
	HTTPTransport     http.RoundTripper
	Log               logr.Logger
	Tracer            trace.Tracer
	TraceServiceName  string
}

// newConnector creates a connector with the given configuration.
func newConnector(cfg connectorConfig) *connector {
	transport := cfg.HTTPTransport
	if transport == nil {
		transport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: cfg.PerAttemptTimeout}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: cfg.PerAttemptTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	return &connector{
		strategy:          cfg.Strategy,
		namespace:         cfg.Namespace,
		serverPort:        cfg.ServerPort,
		requestTimeout:    cfg.RequestTimeout,
		perAttemptTimeout: cfg.PerAttemptTimeout,
		ownsTransport:     cfg.HTTPTransport == nil,
		httpClient: &http.Client{
			Transport: transport,
		},
		log:     cfg.Log,
		tracer:  cfg.Tracer,
		svcName: cfg.TraceServiceName,
	}
}

// SetIdentity sets the claim name used in request headers. Called by Sandbox
// after claim creation.
func (c *connector) SetIdentity(claimName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claimName = claimName
}

// Connect delegates to the strategy to discover and set the base URL.
func (c *connector) Connect(ctx context.Context) error {
	url, err := c.strategy.Connect(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.baseURL = url
	c.lastError = nil
	c.mu.Unlock()
	mode := "direct"
	switch c.strategy.(type) {
	case *gatewayStrategy:
		mode = "gateway"
	case *tunnelStrategy:
		mode = "port-forward"
	}
	c.log.Info("API URL discovered", "url", url, "mode", mode)
	return nil
}

// Close clears state (so concurrent SendRequests see ErrNotReady
// immediately) then tears down the strategy.
func (c *connector) Close() error {
	c.mu.Lock()
	c.baseURL = ""
	c.lastError = nil
	c.claimName = ""
	c.mu.Unlock()
	err := c.strategy.Close()
	if c.ownsTransport {
		c.httpClient.CloseIdleConnections()
	}
	return err
}

// IsConnected returns true if a base URL is set.
func (c *connector) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL != ""
}

// BaseURL returns the current base URL.
func (c *connector) BaseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseURL
}

// SetLastError is called by tunnelStrategy's monitor to signal tunnel death.
func (c *connector) SetLastError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err
	c.baseURL = ""
}

// cancelOnClose wraps an io.ReadCloser to run a cleanup function when the
// caller closes the response body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (cc *cancelOnClose) Close() error {
	err := cc.ReadCloser.Close()
	cc.cancel()
	return err
}

// SendRequest sends an HTTP request to the sandbox router with required
// headers and retry logic for transient failures.
func (c *connector) SendRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string, maxRetries int) (*http.Response, error) {
	limit := maxRetries
	if limit <= 0 {
		limit = maxAttempts
	}

	var requestCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, requestCancel = context.WithTimeout(ctx, c.requestTimeout)
	}
	defer func() {
		if requestCancel != nil {
			requestCancel()
		}
	}()

	reqID := generateRequestID()
	trace.SpanFromContext(ctx).SetAttributes(AttrRequestID.String(reqID))

	var lastErr error
	var lastURL string
	for attempt := range limit {
		c.mu.Lock()
		if c.baseURL == "" {
			pfErr := c.lastError
			claimName := c.claimName
			namespace := c.namespace
			c.mu.Unlock()
			if pfErr != nil {
				return nil, fmt.Errorf("sandbox[%s/%s]: %w (reqID=%s): %w", namespace, claimName, ErrNotReady, reqID, pfErr)
			}
			return nil, fmt.Errorf("sandbox[%s/%s]: %w (reqID=%s)", namespace, claimName, ErrNotReady, reqID)
		}
		reqURL := strings.TrimRight(c.baseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
		lastURL = reqURL
		claimName := c.claimName
		namespace := c.namespace
		port := c.serverPort
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

		attemptCtx, attemptCancel := context.WithCancel(ctx)
		attemptTimer := time.AfterFunc(c.perAttemptTimeout, attemptCancel)

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
			if attempt < limit-1 {
				c.log.V(1).Info("request failed, retrying", "attempt", attempt+1, "maxAttempts", limit, "method", method, "url", reqURL, "reqID", reqID, "error", doErr)
				sleepWithContext(ctx, c.backoff(attempt))
				if ctx.Err() != nil {
					return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
				}
			}
			continue
		}

		if retryableStatusCodes[resp.StatusCode] {
			if attempt >= limit-1 {
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
				_ = resp.Body.Close()
				attemptTimer.Stop()
				attemptCancel()
				httpErr := &HTTPError{StatusCode: resp.StatusCode, Body: string(errBody), Operation: method + " " + endpoint}
				c.log.Error(httpErr, "retries exhausted", "method", method, "url", reqURL, "attempts", limit, "lastStatus", resp.StatusCode, "reqID", reqID, "claim", claimName, "namespace", namespace)
				return nil, fmt.Errorf("%w: %s failed after %d attempts (url=%s reqID=%s): %w",
					ErrRetriesExhausted, method, limit, reqURL, reqID, httpErr)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
			_ = resp.Body.Close()
			attemptTimer.Stop()
			attemptCancel()
			lastErr = fmt.Errorf("sandbox: server returned %d", resp.StatusCode)
			c.log.V(1).Info("retryable status, retrying", "attempt", attempt+1, "maxAttempts", limit, "method", method, "url", reqURL, "status", resp.StatusCode, "reqID", reqID)
			sleepWithContext(ctx, c.backoff(attempt))
			if ctx.Err() != nil {
				return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
			}
			continue
		}

		attemptTimer.Stop()
		// Guard against the rare race where the per-attempt timer fires
		// between Do() returning and Stop(). The response body is tied to
		// the cancelled attemptCtx and is unusable; discard and retry.
		if attemptCtx.Err() != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
			_ = resp.Body.Close()
			attemptCancel()
			if ctx.Err() != nil {
				return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
			}
			lastErr = fmt.Errorf("sandbox: per-attempt timeout raced with response receipt")
			if attempt < limit-1 {
				c.log.V(1).Info("per-attempt timeout race, retrying", "attempt", attempt+1, "maxAttempts", limit, "method", method, "url", reqURL, "reqID", reqID)
				sleepWithContext(ctx, c.backoff(attempt))
				if ctx.Err() != nil {
					return nil, fmt.Errorf("sandbox[%s/%s]: request cancelled (url=%s reqID=%s): %w", namespace, claimName, reqURL, reqID, ctx.Err())
				}
			}
			continue
		}
		rc := requestCancel
		requestCancel = nil
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
	finalNS := c.namespace
	c.mu.Unlock()
	c.log.Error(lastErr, "retries exhausted", "method", method, "url", lastURL, "attempts", limit, "reqID", reqID, "claim", finalClaim, "namespace", finalNS)
	return nil, fmt.Errorf("%w: %s failed after %d attempts (url=%s reqID=%s): %w", ErrRetriesExhausted, method, limit, lastURL, reqID, lastErr)
}

func (c *connector) backoff(attempt int) time.Duration {
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

func generateRequestID() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], rand.Uint64())
	return hex.EncodeToString(b[:])
}

func sleepWithContext(ctx context.Context, d time.Duration) {
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
