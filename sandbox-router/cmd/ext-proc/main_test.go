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

package main

import (
	"context"
	"testing"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TestHealthServerSplitsLivenessAndReadiness pins the contract that
// the default ("") service is SERVING from boot (so the kubelet
// livenessProbe — which targets the empty service name — never
// fails-and-restart-loops while the informer is still syncing on
// large clusters) while the named ext_proc service stays NOT_SERVING
// until WaitForSync completes (so neither Envoy nor the kubelet
// readinessProbe sends real traffic prematurely).
func TestHealthServerSplitsLivenessAndReadiness(t *testing.T) {
	h := newHealthServer()
	ctx := context.Background()

	def, err := h.Check(ctx, &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("default service Check: %v", err)
	}
	if def.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("default service: got %s, want SERVING (liveness must succeed at boot)", def.Status)
	}

	named, err := h.Check(ctx, &healthpb.HealthCheckRequest{Service: healthServiceName})
	if err != nil {
		t.Fatalf("named service Check: %v", err)
	}
	if named.Status != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("named service: got %s, want NOT_SERVING at boot (gated on informer sync)", named.Status)
	}
}
