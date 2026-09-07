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

// inClusterStrategy addresses the sandbox runtime on the pod network, taking
// the apiserver (and, for the legacy runtime, the sandbox-router) off the data
// path. Connectivity picks exactly one address, with no fallback between
// them: useServiceDNS dials the headless Service by name
// (Status.ServiceFQDN), otherwise it dials Status.PodIP.
//
// A caller outside the cluster cannot use it. For the legacy runtime it also gives
// up the router's own validation of the routing headers, which are not sent on this
// path because nothing would consume them.
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

	// useServiceDNS dials the headless Service's DNS name rather than the
	// pod IP. The two are exclusive; neither falls back to the other.
	useServiceDNS bool

	// getServiceFQDN and getPodIP return the resolved Sandbox addresses; set
	// after construction (they are only known once the sandbox is ready).
	// Mirrors podTunnelStrategy.getPodName.
	getServiceFQDN func() string
	getPodIP       func() string

	// connector is set after construction so Connect can publish the gRPC
	// dial target.
	connector *connector
}

// Connect uses resolveHost to get the URL to connect to. When using sandboxd it also
// sets the connector's gRPC target. The method exists so inClusterStrategy conforms
// to the ConnectionStrategy interface.
func (t *inClusterStrategy) Connect(ctx context.Context) (string, error) {
	_, span := startSpan(ctx, t.tracer, t.svcName, "sandboxd_in_cluster")
	defer span.End()

	host, via, err := t.resolveHost()
	if err != nil {
		recordError(span, err)
		return "", err
	}

	baseURL := "http://" + net.JoinHostPort(host, strconv.Itoa(t.httpPort))
	if t.grpcPort != 0 && t.connector != nil {
		t.connector.SetGRPCTarget(net.JoinHostPort(host, strconv.Itoa(t.grpcPort)))
	}
	t.log.V(1).Info("in-cluster transport resolved",
		"host", host, "via", via, "httpPort", t.httpPort, "grpcPort", t.grpcPort)
	return baseURL, nil
}

// resolveHost returns the address this strategy dials either the service FQDN
// or the pod IP, and a label for logging.
func (t *inClusterStrategy) resolveHost() (host, via string, err error) {
	if t.useServiceDNS {
		fqdn := ""
		if t.getServiceFQDN != nil {
			fqdn = t.getServiceFQDN()
		}
		if fqdn == "" {
			return "", "", fmt.Errorf("sandbox: %w: cannot address it by DNS; set spec.service: true on the template, or use %q connectivity to dial the pod IP", ErrNoSandboxService, ConnectivityInClusterPodIP)
		}
		return fqdn, "service", nil
	}

	podIP := ""
	if t.getPodIP != nil {
		podIP = t.getPodIP()
	}
	if podIP == "" {
		return "", "", fmt.Errorf("sandbox: sandbox pod IP not resolved yet; cannot connect directly")
	}
	return podIP, "pod-ip", nil
}

// Close is a no-op: the strategy owns no connections or goroutines. The HTTP
// and gRPC clients it hands addresses to are torn down by the connector.
func (t *inClusterStrategy) Close() error { return nil }
