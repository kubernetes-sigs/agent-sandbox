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
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
)

const maxStderrSize = 64 << 10 // 64 KB

// syncBuffer is a goroutine-safe, size-capped bytes.Buffer for port-forward stderr.
type syncBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxStderrSize - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		b.buf.Write(p)
	} else {
		b.truncated = true
	}
	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "... [truncated at 64KB]"
	}
	return b.buf.String()
}

// trackingDialer wraps an httpstream.Dialer to track the streaming connection so it
// can be forcefully closed when the stop channel alone does not unblock ForwardPorts.
type trackingDialer struct {
	inner  httpstream.Dialer
	mu     sync.Mutex
	conn   httpstream.Connection
	closed bool
}

func (d *trackingDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	conn, proto, err := d.inner.Dial(protocols...)
	if err != nil {
		return nil, "", err
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		conn.Close()
		return nil, "", fmt.Errorf("sandbox: port-forward was stopped during connection setup")
	}
	d.conn = conn
	d.mu.Unlock()
	return conn, proto, nil
}

func (d *trackingDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// tunnelStrategy establishes a port-forward tunnel to the sandbox-router
// service using native SPDY via client-go.
type tunnelStrategy struct {
	discoveryClient discoveryv1client.DiscoveryV1Interface
	namespace       string
	log             logr.Logger
	tracer          trace.Tracer
	svcName         string

	tunnel *portForwardTunnel
}

func (t *tunnelStrategy) Connect(ctx context.Context) (string, error) {
	ctx, span := startSpan(ctx, t.tracer, t.svcName, "dev_mode_tunnel")
	defer span.End()

	if t.tunnel == nil || t.tunnel.coreClient == nil || t.tunnel.restConfig == nil {
		err := fmt.Errorf("sandbox: core client and REST config required for port-forward")
		recordError(span, err)
		return "", err
	}

	// Stop any existing port-forward to prevent goroutine leaks on reconnect.
	t.tunnel.stop()

	podName, err := t.resolveRouterPod(ctx)
	if err != nil {
		recordError(span, err)
		return "", err
	}

	var baseURL string
	onReady := func(ports []portforward.ForwardedPort) error {
		if len(ports) == 0 {
			return fmt.Errorf("sandbox: port forwarder reported no forwarded ports")
		}

		baseURL = fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
		t.log.Info("port-forward established",
			"localPort", ports[0].Local, "pod", podName)
		return nil
	}

	err = t.tunnel.start(
		ctx, podName, []string{"0:8080"}, "port-forward", onReady,
	)
	if err != nil {
		recordError(span, err)
		return "", err
	}

	return baseURL, nil
}

func (t *tunnelStrategy) Close() error {
	return t.tunnel.Close()
}

func (t *tunnelStrategy) resolveRouterPod(ctx context.Context) (string, error) {
	if t.discoveryClient == nil {
		return "", fmt.Errorf("sandbox: discovery client required for port-forward")
	}

	selector := "kubernetes.io/service-name=sandbox-router-svc"
	slices, err := t.discoveryClient.EndpointSlices(t.namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: selector},
	)
	if err != nil {
		return "", fmt.Errorf("sandbox: failed to list EndpointSlices for sandbox-router-svc: %w", err)
	}

	for _, slice := range slices.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.TargetRef != nil && ep.TargetRef.Name != "" {
				return ep.TargetRef.Name, nil
			}
		}
	}

	return "", fmt.Errorf("sandbox: no ready endpoints for sandbox-router-svc in namespace %s", t.namespace)
}
