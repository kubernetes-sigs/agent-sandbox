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

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/tools/portforward"
)

// podTunnelStrategy establishes a SPDY port-forward directly to the sandbox
// pod, carrying both sandboxd listeners: the Filesystem & Runtime REST API
// and the gRPC ProcessService. It is the default sandboxd transport because
// the tunnel is brokered by the apiserver, so it works from a laptop or CI
// runner with no route to pod IPs, and it does not need a sandbox-router.
// Clients with pod-network access can still connect to sandboxd directly.
//
// Callers can go for a shorter path with inClusterStrategy (incluster.go),
// which dials the pod IP directly and keeps the apiserver off the data path.
//
// Structure mirrors tunnelStrategy (tunnel.go), which forwards to the
// sandbox-router service for the legacy runtime instead.
type podTunnelStrategy struct {
	restPort  int
	grpcPort  int
	log       logr.Logger
	tracer    trace.Tracer
	svcName   string
	connector *connector

	// getPodName returns the resolved sandbox pod name; set after
	// construction (the pod is only known once the sandbox is ready).
	getPodName func() string

	tunnel *portForwardTunnel
}

func (t *podTunnelStrategy) Connect(ctx context.Context) (string, error) {
	ctx, span := startSpan(ctx, t.tracer, t.svcName, "sandboxd_pod_tunnel")
	defer span.End()

	if t.tunnel == nil || t.tunnel.coreClient == nil || t.tunnel.restConfig == nil {
		err := fmt.Errorf("sandbox: core client and REST config required for pod port-forward")
		recordError(span, err)
		return "", err
	}
	podName := ""
	if t.getPodName != nil {
		podName = t.getPodName()
	}
	if podName == "" {
		err := fmt.Errorf("sandbox: sandbox pod name not resolved yet; cannot port-forward")
		recordError(span, err)
		return "", err
	}

	// Stop any existing port-forward to prevent goroutine leaks on reconnect.
	t.tunnel.stop()

	forwardSpecs := []string{
		fmt.Sprintf("0:%d", t.restPort),
		fmt.Sprintf("0:%d", t.grpcPort),
	}

	var baseURL string
	onReady := func(ports []portforward.ForwardedPort) error {
		var restLocal, grpcLocal uint16
		for _, p := range ports {
			switch int(p.Remote) {
			case t.restPort:
				restLocal = p.Local
			case t.grpcPort:
				grpcLocal = p.Local
			}
		}
		if restLocal == 0 || grpcLocal == 0 {
			return fmt.Errorf(
				"sandbox: port forwarder did not report both sandboxd ports (rest=%d grpc=%d)",
				restLocal, grpcLocal,
			)
		}

		baseURL = fmt.Sprintf("http://127.0.0.1:%d", restLocal)
		grpcTarget := fmt.Sprintf("127.0.0.1:%d", grpcLocal)
		if t.connector != nil {
			t.connector.SetGRPCTarget(grpcTarget)
		}

		t.log.Info("sandboxd pod port-forward established",
			"pod", podName,
			"restLocalPort", restLocal,
			"grpcLocalPort", grpcLocal)
		return nil
	}

	err := t.tunnel.start(
		ctx, podName, forwardSpecs, "pod port-forward", onReady,
	)
	if err != nil {
		recordError(span, err)
		return "", err
	}
	return baseURL, nil
}

func (t *podTunnelStrategy) Close() error {
	return t.tunnel.Close()
}
