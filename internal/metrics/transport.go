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

// nolint:revive
package metrics

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Client-side transport observability for the Kubernetes API client.
//
// kube-apiserver caps concurrent in-flight requests per HTTP/2 connection
// (SETTINGS_MAX_CONCURRENT_STREAMS; it advertises 100 unless
// --http2-max-streams-per-connection is set — see
// k8s.io/apiserver/pkg/server/secure_serving.go), and client-go multiplexes
// all requests from one rest.Config onto (effectively) a single connection.
// When that connection saturates, requests queue *client-side* before they
// are ever sent, which is invisible to every server-side metric:
// apiserver_request_duration_seconds stays flat while the controller
// observes ballooning RTTs.
//
// The direct signal is the time between starting a request and the
// transport actually acquiring a usable connection for it (httptrace
// GotConn). On a healthy client this is ~0 (the pooled connection has free
// stream slots); under stream-slot saturation it is exactly the client-side
// queue time.
var (
	// RestClientConnWait measures the time from the start of a Kubernetes
	// API request until the underlying transport acquired the connection the
	// request was actually sent on (httptrace GotConn).
	// Labels:
	// - method: HTTP method (GET, POST, PUT, PATCH, DELETE).
	// - reused: whether the acquired connection was reused ("true") or
	//   freshly dialed ("false").
	RestClientConnWait = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "rest_client_transport_conn_wait_seconds",
			Help: "Time from request start until the transport acquired a connection for it (httptrace GotConn). " +
				"Sustained growth here while apiserver_request_duration_seconds stays flat indicates client-side " +
				"queueing for HTTP/2 stream slots (SETTINGS_MAX_CONCURRENT_STREAMS saturation).",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "reused"},
	)

	// RestClientDials counts underlying TCP connection attempts to the API
	// server (httptrace ConnectDone). A busy controller should sit at ~0
	// dials/s; a sustained dial rate under load is the fingerprint of the
	// transport repeatedly trying to escape a saturated connection (net/http
	// dials a new connection on ErrNoCachedConn, and the HTTP/2 pool then
	// discards it if the old connection freed a slot meanwhile).
	// Labels:
	// - result: "success" or "error".
	RestClientDials = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rest_client_transport_dials_total",
			Help: "Total TCP connection attempts by the Kubernetes API client transport, by result.",
		},
		[]string{"result"},
	)

	// RestClientInflightRequests tracks requests that currently hold an
	// HTTP/2 stream (from RoundTrip start until the response body is
	// closed, so long-lived watch streams are counted for their lifetime).
	// A plateau at ~100 (the kube-apiserver per-connection stream cap)
	// while workqueue depth grows means the client is connection-bound.
	RestClientInflightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "rest_client_transport_inflight_requests",
			Help: "In-flight Kubernetes API requests, counted from RoundTrip start until the response body is closed.",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(RestClientConnWait)
	metrics.Registry.MustRegister(RestClientDials)
	metrics.Registry.MustRegister(RestClientInflightRequests)
}

// InstrumentRESTConfig installs the transport instrumentation on cfg via
// cfg.Wrap, composing with any wrapper already present. Every client built
// from cfg (manager cache list/watches, manager client, event recorder,
// leader election) is observed.
func InstrumentRESTConfig(cfg *rest.Config) {
	cfg.Wrap(NewTransportMetricsRoundTripper)
}

// NewTransportMetricsRoundTripper wraps rt with the rest_client_transport_*
// instrumentation.
func NewTransportMetricsRoundTripper(rt http.RoundTripper) http.RoundTripper {
	return &transportMetricsRoundTripper{next: rt}
}

type transportMetricsRoundTripper struct {
	next http.RoundTripper
}

func (t *transportMetricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	acq := &connAcquisition{start: time.Now()}
	trace := &httptrace.ClientTrace{
		GotConn: acq.gotConn,
		ConnectDone: func(_, _ string, err error) {
			result := "success"
			if err != nil {
				result = "error"
			}
			RestClientDials.WithLabelValues(result).Inc()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	RestClientInflightRequests.Inc()
	resp, err := t.next.RoundTrip(req)

	// Observe the wait for the connection the request was finally sent on.
	// GotConn can fire more than once per request (net/http retries with a
	// fresh dial when the pooled HTTP/2 connection is at its stream limit),
	// so the last acquisition is the one that carried the request.
	if wait, reused, ok := acq.snapshot(); ok {
		RestClientConnWait.WithLabelValues(req.Method, strconv.FormatBool(reused)).Observe(wait.Seconds())
	}

	if err != nil {
		RestClientInflightRequests.Dec()
		return nil, err
	}
	resp.Body = &inflightTrackingBody{ReadCloser: resp.Body}
	return resp, nil
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// wrapped transport (net/http checks for this interface).
func (t *transportMetricsRoundTripper) CloseIdleConnections() {
	if ci, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

// WrappedRoundTripper implements k8s.io/apimachinery/pkg/util/net.RoundTripperWrapper.
func (t *transportMetricsRoundTripper) WrappedRoundTripper() http.RoundTripper {
	return t.next
}

// connAcquisition records the most recent GotConn event for one request.
// Trace callbacks can fire from transport-internal goroutines, hence the
// mutex; they always complete before RoundTrip returns the connection's
// response.
type connAcquisition struct {
	start time.Time

	mu     sync.Mutex
	wait   time.Duration
	reused bool
	seen   bool
}

func (a *connAcquisition) gotConn(info httptrace.GotConnInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.wait = time.Since(a.start)
	a.reused = info.Reused
	a.seen = true
}

func (a *connAcquisition) snapshot() (time.Duration, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wait, a.reused, a.seen
}

// inflightTrackingBody decrements the in-flight gauge when the response
// body is closed (which is when the HTTP/2 stream is released).
type inflightTrackingBody struct {
	io.ReadCloser
	once sync.Once
}

func (b *inflightTrackingBody) Close() error {
	b.once.Do(RestClientInflightRequests.Dec)
	return b.ReadCloser.Close()
}
