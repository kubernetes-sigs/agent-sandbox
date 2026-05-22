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

package server

import (
	"net/http"
	"sync/atomic"
)

// Probes serves /healthz and /readyz. Liveness (/healthz) is always 200.
// Readiness (/readyz) flips to 503 once MarkUnready is called, which lets
// load balancers drain the pod ahead of shutdown.
type Probes struct {
	ready atomic.Bool
}

// NewProbes returns a Probes initialized to not-yet-ready. Call MarkReady
// once the proxy listener is accepting connections.
func NewProbes() *Probes {
	return &Probes{}
}

// MarkReady advertises the pod as ready.
func (p *Probes) MarkReady() {
	p.ready.Store(true)
}

// MarkUnready forces /readyz to 503.
func (p *Probes) MarkUnready() {
	p.ready.Store(false)
}

// Healthz always returns 200 OK with the Python-compatible JSON body so
// tooling that parses {"status":"ok"} keeps working.
func (p *Probes) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Readyz returns 200 if ready, 503 otherwise.
func (p *Probes) Readyz(w http.ResponseWriter, _ *http.Request) {
	if p.ready.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("draining"))
}

// Mux returns a mux serving /healthz and /readyz from p.
func (p *Probes) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", p.Healthz)
	mux.HandleFunc("/readyz", p.Readyz)
	return mux
}
