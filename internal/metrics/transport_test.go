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
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// histogramTotals gathers the total sample count and sum across all label
// combinations of vec.
func histogramTotals(t *testing.T, vec *prometheus.HistogramVec) (count uint64, sum float64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(vec))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			count += m.GetHistogram().GetSampleCount()
			sum += m.GetHistogram().GetSampleSum()
		}
	}
	return count, sum
}

func TestTransportMetricsRoundTripper(t *testing.T) {
	RestClientConnWait.Reset()
	RestClientDials.Reset()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	inflightBefore := testutil.ToFloat64(RestClientInflightRequests)
	client := &http.Client{Transport: NewTransportMetricsRoundTripper(&http.Transport{})}

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)

	// The response body is still open, so the request holds its slot.
	require.InDelta(t, inflightBefore+1, testutil.ToFloat64(RestClientInflightRequests), 0)

	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	// Double-close must not double-decrement.
	require.NoError(t, resp.Body.Close())
	require.InDelta(t, inflightBefore, testutil.ToFloat64(RestClientInflightRequests), 0)

	count, _ := histogramTotals(t, RestClientConnWait)
	require.Equal(t, uint64(1), count, "expected exactly one conn-wait observation")

	require.GreaterOrEqual(t, testutil.ToFloat64(RestClientDials.WithLabelValues("success")), 1.0,
		"expected at least one successful dial to be counted")
}

func TestConnWaitMeasuresConnectionQueueing(t *testing.T) {
	RestClientConnWait.Reset()

	const handlerDelay = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(handlerDelay)
		_, _ = w.Write([]byte("slow"))
	}))
	defer srv.Close()

	// One connection for two concurrent slow requests: the second request
	// must wait for the first to release the connection, and that wait is
	// what the conn-wait histogram exists to expose.
	client := &http.Client{Transport: NewTransportMetricsRoundTripper(&http.Transport{MaxConnsPerHost: 1})}

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		})
	}
	wg.Wait()

	count, sum := histogramTotals(t, RestClientConnWait)
	require.Equal(t, uint64(2), count)
	require.GreaterOrEqual(t, sum, (handlerDelay / 2).Seconds(),
		"the queued request's conn wait should reflect the time spent waiting for a free connection")
}

func TestTransportMetricsErrorPath(t *testing.T) {
	inflightBefore := testutil.ToFloat64(RestClientInflightRequests)

	// A just-closed listener's port refuses connections immediately.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedAddr := l.Addr().String()
	require.NoError(t, l.Close())

	client := &http.Client{Transport: NewTransportMetricsRoundTripper(&http.Transport{})}
	_, err = client.Get("http://" + refusedAddr)
	require.Error(t, err)

	require.GreaterOrEqual(t, testutil.ToFloat64(RestClientDials.WithLabelValues("error")), 1.0,
		"the refused dial should be counted as an error")

	require.InDelta(t, inflightBefore, testutil.ToFloat64(RestClientInflightRequests), 0,
		"a failed request must not leak the in-flight gauge")
}
