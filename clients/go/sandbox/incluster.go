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
	"fmt"
	"net"
	"strconv"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
)

// inClusterStrategy addresses the sandbox runtime on the pod's IP, taking
// the apiserver (and, for the legacy runtime, the sandbox-router) off the data
// path.
//
// A caller outside the cluster cannot use it. For the legacy runtime it also gives
// up the router's own validation of the routing headers, which are not sent on this
// path because nothing would consume them.
//
// Pod IP staleness is not handled here, and recovery differs from the
// port-forward's. podTunnelStrategy runs a monitor that detects tunnel death
// and calls connector.SetLastError, which clears the base URL so operations
// fail fast with ErrNotReady and a plain Open() reconnects. A direct dial has
// no equivalent signal: nothing clears the base URL, so after the sandbox pod
// is rescheduled the connector still reports IsConnected, requests keep
// failing against the dead IP with ErrRetriesExhausted rather than
// ErrNotReady, and Open() returns ErrAlreadyOpen instead of reconnecting.
// Recovery is therefore Disconnect() followed by Open(): Connect re-reads
// getPodIP on every call, so the fresh address is picked up then.
//
// An IP the cluster has since reassigned to an unrelated pod would not be
// detected at all — the SDK has no identity check on this path today, the
// same as for the port-forward.
type inClusterStrategy struct {
	// httpPort carries the runtime's HTTP API: sandboxd's Filesystem &
	// Runtime REST port, or the legacy runtime's ServerPort.
	httpPort int
	// grpcPort is sandboxd's ProcessService port, or 0 for a runtime that
	// serves no gRPC (the legacy runtime).
	grpcPort int
	log      logr.Logger
	tracer   trace.Tracer
	svcName  string

	// getPodIP returns the resolved sandbox pod IP; set after construction
	// (the pod is only known once the sandbox is ready). Mirrors
	// podTunnelStrategy.getPodName.
	getPodIP func() string

	// connector is set after construction so Connect can publish the gRPC
	// dial target.
	connector *connector
}

func (t *inClusterStrategy) Connect(ctx context.Context) (string, error) {
	_, span := startSpan(ctx, t.tracer, t.svcName, "sandboxd_in_cluster")
	defer span.End()

	podIP := ""
	if t.getPodIP != nil {
		podIP = t.getPodIP()
	}
	if podIP == "" {
		err := fmt.Errorf("sandbox: sandbox pod IP not resolved yet; cannot connect directly")
		recordError(span, err)
		return "", err
	}

	// JoinHostPort brackets IPv6 literals, which both the URL and the gRPC
	// dial target require. The IP is already normalized by selectPodIP when
	// the sandbox status is read.
	baseURL := "http://" + net.JoinHostPort(podIP, strconv.Itoa(t.httpPort))
	if t.grpcPort != 0 && t.connector != nil {
		t.connector.SetGRPCTarget(net.JoinHostPort(podIP, strconv.Itoa(t.grpcPort)))
	}
	t.log.V(1).Info("in-cluster transport resolved",
		"podIP", podIP, "httpPort", t.httpPort, "grpcPort", t.grpcPort)
	return baseURL, nil
}

// Close is a no-op: the strategy owns no connections or goroutines. The HTTP
// and gRPC clients it hands addresses to are torn down by the connector.
func (t *inClusterStrategy) Close() error { return nil }
