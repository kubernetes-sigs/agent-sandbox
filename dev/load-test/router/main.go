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

// Binary router-load drives synthetic load through the Go sandbox-router
// to a no-op in-process backend so capacity numbers can be captured without
// spinning up a cluster. It prints throughput + latency percentiles in a
// shape that is easy to paste into the package README.
//
// Usage:
//
//	go run ./dev/load-test/router \
//	    --router-url=http://127.0.0.1:8080 \
//	    --concurrency=50 \
//	    --duration=30s
//
// To drive an in-process router (no external binary), pass --in-process.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/proxy"
)

func main() {
	routerURL := flag.String("router-url", "",
		"URL of an already-running sandbox-router. Mutually exclusive with --in-process.")
	inProcess := flag.Bool("in-process", false,
		"Spin up a router in-process and a no-op backend; ignores --router-url.")
	concurrency := flag.Int("concurrency", 50,
		"Number of worker goroutines firing requests in parallel.")
	duration := flag.Duration("duration", 15*time.Second,
		"How long to drive load before stopping.")
	bodySize := flag.Int("body-size", 0,
		"Bytes of request body to send (default 0 = GET-style empty body).")
	warmup := flag.Duration("warmup", 2*time.Second,
		"How long to drive load before starting to record samples.")
	backendHost := flag.String("backend-host", "",
		"Sandbox Pod IP to send in X-Sandbox-Pod-IP. Required with --router-url; "+
			"populated automatically from the in-process httptest backend when --in-process is set.")
	backendPort := flag.Int("backend-port", 0,
		"Sandbox port to send in X-Sandbox-Port. Required with --router-url; "+
			"populated automatically from the in-process httptest backend when --in-process is set.")
	flag.Parse()

	if *routerURL == "" && !*inProcess {
		fmt.Fprintln(os.Stderr, "either --router-url or --in-process is required")
		flag.Usage()
		os.Exit(2)
	}
	if *bodySize < 0 {
		// make([]byte, negative) panics; surface the bad input as a usage
		// error instead.
		fmt.Fprintf(os.Stderr, "--body-size must be >= 0, got %d\n", *bodySize)
		flag.Usage()
		os.Exit(2)
	}

	var (
		target   string
		backend  *httptest.Server
		cleanups []func()
	)
	if *inProcess {
		target, backend, cleanups = startInProcess()
		defer func() {
			for _, c := range cleanups {
				c()
			}
		}()
		fmt.Printf("in-process router at %s, backend at %s\n", target, backend.URL)
		// Only fill in from the httptest backend if the operator didn't
		// pin specific values — lets in-process mode also test against
		// a custom backend address.
		if *backendHost == "" || *backendPort == 0 {
			if h, p, err := splitHostPort(backend.URL); err == nil {
				if *backendHost == "" {
					*backendHost = h
				}
				if *backendPort == 0 {
					if n, err := strconv.Atoi(p); err == nil {
						*backendPort = n
					}
				}
			}
		}
	} else {
		target = *routerURL
		// External mode: the router validates X-Sandbox-Pod-IP and
		// X-Sandbox-Port, so both must be set to something the router
		// will accept. Without these, every request 400's before
		// touching the proxy code path we're trying to measure.
		if *backendHost == "" {
			fmt.Fprintln(os.Stderr, "--backend-host is required with --router-url")
			flag.Usage()
			os.Exit(2)
		}
		if *backendPort == 0 {
			fmt.Fprintln(os.Stderr, "--backend-port is required with --router-url")
			flag.Usage()
			os.Exit(2)
		}
	}

	// Parse the target once so a malformed --router-url fails here with
	// a clear message rather than crashing every worker goroutine on a
	// nil http.Request later. The in-process path always produces a
	// valid URL (httptest.NewServer.URL), so this just guards the
	// external-mode case.
	if _, err := url.Parse(target + "/load"); err != nil {
		fmt.Fprintf(os.Stderr, "invalid target URL %q: %v\n", target, err)
		flag.Usage()
		os.Exit(2)
	}

	results := run(target, *concurrency, *duration, *warmup, *bodySize, *backendHost, *backendPort)
	results.Print()
}

// startInProcess wires up a no-op backend + an in-process router and returns
// the router URL plus cleanup funcs.
func startInProcess() (string, *httptest.Server, []func()) {
	// No-op backend: returns 200 immediately. Tight loop, no allocations of
	// note, so the router is the bottleneck rather than the backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	cfg := config.Defaults()
	cfg.ProxyTimeout = 30 * time.Second
	cfg.ResponseHeaderTimeout = 5 * time.Second
	// Retries are 0 here so a single dial failure doesn't skew latency.
	cfg.UpstreamMaxRetries = 0
	// The in-process backend is an httptest server on 127.0.0.1; the
	// router's default SSRF guard rejects loopback X-Sandbox-Pod-IP
	// without this opt-in.
	cfg.AllowLoopbackPodIP = true

	handler := proxy.NewHandler(proxy.Options{
		Config: &cfg,
		Logger: logr.Discard(),
	})
	router := httptest.NewServer(handler)

	return router.URL, backend, []func(){router.Close, backend.Close}
}

type sample struct {
	dur    time.Duration
	status int
}

type results struct {
	samples     []sample
	totalSent   int64
	totalErrors int64
	elapsed     time.Duration
	concurrency int
}

func run(target string, concurrency int, duration, warmup time.Duration, bodySize int, backendHost string, backendPort int) results {
	// Tune the client for high concurrency: enough idle conns, generous timeouts.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        concurrency * 4,
			MaxIdleConnsPerHost: concurrency * 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration+warmup)
	defer cancel()

	// Recording starts after warmup so the first cold-start tail doesn't
	// pollute percentiles.
	recordingStart := time.Now().Add(warmup)

	var (
		samples     []sample
		samplesMu   sync.Mutex
		totalSent   atomic.Int64
		totalErrors atomic.Int64
	)

	bodyTemplate := make([]byte, bodySize)
	for i := range bodyTemplate {
		bodyTemplate[i] = byte('a' + (i % 26))
	}
	backendPortStr := strconv.Itoa(backendPort)

	wg := sync.WaitGroup{}
	wg.Add(concurrency)
	start := time.Now()
	for range concurrency {
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req := buildReq(target, backendHost, backendPortStr, bodyTemplate)
				t0 := time.Now()
				resp, err := client.Do(req)
				dur := time.Since(t0)
				totalSent.Add(1)
				if err != nil {
					totalErrors.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				// Record only after warmup window.
				if time.Now().After(recordingStart) {
					samplesMu.Lock()
					samples = append(samples, sample{dur: dur, status: resp.StatusCode})
					samplesMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	return results{
		samples:     samples,
		totalSent:   totalSent.Load(),
		totalErrors: totalErrors.Load(),
		elapsed:     time.Since(start),
		concurrency: concurrency,
	}
}

func buildReq(target, backendHost, backendPort string, body []byte) *http.Request {
	var bodyReader io.Reader
	method := http.MethodGet
	if len(body) > 0 {
		bodyReader = newRepeatingReader(body)
		method = http.MethodPost
	}
	// target was pre-validated in main(); a NewRequest error here would
	// mean the inputs changed under us — a panic gives an immediate
	// stack trace rather than a worker silently looping on nil.
	req, err := http.NewRequest(method, target+"/load", bodyReader)
	if err != nil {
		panic(fmt.Sprintf("buildReq: unexpected http.NewRequest error for target %q: %v", target, err))
	}
	req.Header.Set("X-Sandbox-Id", "loadtest")
	req.Header.Set("X-Sandbox-Namespace", "loadtest")
	req.Header.Set("X-Sandbox-Pod-Ip", backendHost)
	req.Header.Set("X-Sandbox-Port", backendPort)
	return req
}

// newRepeatingReader returns a reader that yields the given bytes once.
// Kept simple — load testing with bodies just needs SOMETHING to send, not
// gigabyte-scale streaming.
func newRepeatingReader(b []byte) io.Reader {
	return &oneshotReader{b: b}
}

type oneshotReader struct {
	b []byte
	i int
}

func (r *oneshotReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func splitHostPort(u string) (string, string, error) {
	// Strip scheme + path.
	if i := indexOf(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := indexOf(u, "/"); i >= 0 {
		u = u[:i]
	}
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return "", "", err
	}
	return host, port, nil
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Print writes a one-block summary suitable for pasting into a README.
func (r results) Print() {
	slices.SortFunc(r.samples, func(a, b sample) int {
		switch {
		case a.dur < b.dur:
			return -1
		case a.dur > b.dur:
			return 1
		}
		return 0
	})

	statuses := map[int]int{}
	for _, s := range r.samples {
		statuses[s.status]++
	}

	fmt.Println()
	fmt.Println("=== sandbox-router load test ===")
	fmt.Printf("  concurrency:      %d\n", r.concurrency)
	fmt.Printf("  elapsed:          %s\n", r.elapsed.Round(time.Millisecond))
	fmt.Printf("  total requests:   %d\n", r.totalSent)
	fmt.Printf("  client errors:    %d\n", r.totalErrors)
	fmt.Printf("  recorded samples: %d  (warmup-excluded)\n", len(r.samples))
	if r.elapsed > 0 {
		fmt.Printf("  throughput:       %.0f req/s\n", float64(r.totalSent)/r.elapsed.Seconds())
	}
	fmt.Println("  status codes:")
	for code, n := range statuses {
		fmt.Printf("    %d: %d\n", code, n)
	}
	if len(r.samples) == 0 {
		return
	}
	fmt.Println("  latency (recorded samples only):")
	fmt.Printf("    p50:  %s\n", pct(r.samples, 50))
	fmt.Printf("    p90:  %s\n", pct(r.samples, 90))
	fmt.Printf("    p95:  %s\n", pct(r.samples, 95))
	fmt.Printf("    p99:  %s\n", pct(r.samples, 99))
	fmt.Printf("    p999: %s\n", pct(r.samples, 99.9))
	fmt.Printf("    max:  %s\n", r.samples[len(r.samples)-1].dur.Round(time.Microsecond))
}

func pct(s []sample, p float64) time.Duration {
	if len(s) == 0 {
		return 0
	}
	idx := int(float64(len(s)) * p / 100)
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx].dur.Round(time.Microsecond)
}
