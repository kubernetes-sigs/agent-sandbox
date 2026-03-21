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
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
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

// trackingDialer wraps an httpstream.Dialer to track the SPDY connection so it
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
	coreClient      corev1client.CoreV1Interface
	discoveryClient discoveryv1client.DiscoveryV1Interface
	restConfig      *rest.Config
	namespace       string
	pfTimeout       time.Duration
	log             logr.Logger
	tracer          trace.Tracer
	svcName         string

	// Runtime state.
	portForwardStopChan chan struct{}
	spdyUpgradeClient   *http.Client
	pfDialer            *trackingDialer
	mu                  sync.Mutex

	// connector is set after construction so the monitor can signal death.
	connector *connector
}

func (t *tunnelStrategy) Connect(ctx context.Context) (string, error) {
	ctx, span := startSpan(ctx, t.tracer, t.svcName, "dev_mode_tunnel")
	defer span.End()

	if t.coreClient == nil || t.restConfig == nil {
		err := fmt.Errorf("sandbox: core client and REST config required for port-forward")
		recordError(span, err)
		return "", err
	}

	// Stop any existing port-forward to prevent goroutine leaks on reconnect.
	t.mu.Lock()
	t.stopPortForward()
	t.mu.Unlock()

	podName, err := t.resolveRouterPod(ctx)
	if err != nil {
		recordError(span, err)
		return "", err
	}

	reqURL := t.coreClient.RESTClient().Post().
		Resource("pods").
		Namespace(t.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(t.restConfig)
	if err != nil {
		recordError(span, err)
		return "", fmt.Errorf("sandbox: failed to create SPDY round tripper: %w", err)
	}
	spdyClient := &http.Client{
		Transport: transport,
		Timeout:   t.pfTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	td := &trackingDialer{inner: spdy.NewDialer(upgrader, spdyClient, http.MethodPost, reqURL)}

	t.mu.Lock()
	if t.spdyUpgradeClient != nil {
		t.spdyUpgradeClient.CloseIdleConnections()
	}
	t.spdyUpgradeClient = spdyClient
	t.pfDialer = td
	t.mu.Unlock()

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	var stderrBuf syncBuffer
	fw, err := portforward.New(td, []string{"0:8080"}, stopChan, readyChan, io.Discard, &stderrBuf)
	if err != nil {
		recordError(span, err)
		return "", fmt.Errorf("sandbox: failed to create port forwarder: %w", err)
	}

	errChan := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errChan <- fmt.Errorf("sandbox: port-forward panicked: %v", r)
			}
		}()
		errChan <- fw.ForwardPorts()
	}()

	pfCtx, pfCancel := context.WithTimeout(ctx, t.pfTimeout)
	defer pfCancel()

	select {
	case <-readyChan:
		ports, err := fw.GetPorts()
		if err != nil {
			close(stopChan)
			td.Close()
			recordError(span, err)
			return "", fmt.Errorf("sandbox: failed to get forwarded ports: %w", err)
		}
		if len(ports) == 0 {
			close(stopChan)
			td.Close()
			err := fmt.Errorf("sandbox: port forwarder reported no forwarded ports")
			recordError(span, err)
			return "", err
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
		t.mu.Lock()
		t.portForwardStopChan = stopChan
		t.mu.Unlock()
		t.log.Info("port-forward established", "localPort", ports[0].Local, "pod", podName)
		go t.monitorPortForward(errChan, &stderrBuf, podName, stopChan)
		return baseURL, nil
	case err := <-errChan:
		td.Close() // release SPDY connection established during Dial
		if stderr := stderrBuf.String(); stderr != "" {
			retErr := fmt.Errorf("sandbox: port-forward failed: %w (stderr: %s)", err, stderr)
			recordError(span, retErr)
			return "", retErr
		}
		recordError(span, err)
		return "", fmt.Errorf("sandbox: port-forward failed: %w", err)
	case <-pfCtx.Done():
		close(stopChan)
		td.Close()
		go func() {
			timer := time.NewTimer(monitorExitTimeout)
			defer timer.Stop()
			select {
			case <-errChan:
			case <-timer.C:
				t.log.Error(nil, "port-forward goroutine did not exit after cancellation; resources may be leaked", "pod", podName, "timeout", monitorExitTimeout)
			}
		}()
		retErr := fmt.Errorf("sandbox: port-forward cancelled: %w", pfCtx.Err())
		recordError(span, retErr)
		return "", retErr
	}
}

func (t *tunnelStrategy) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopPortForward()
	return nil
}

const monitorExitTimeout = 5 * time.Second
const maxLastErrorStderr = 256

func (t *tunnelStrategy) monitorPortForward(errChan <-chan error, stderrBuf *syncBuffer, podName string, myStopChan chan struct{}) {
	var err error
	select {
	case err = <-errChan:
	case <-myStopChan:
		timer := time.NewTimer(monitorExitTimeout)
		defer timer.Stop()
		select {
		case err = <-errChan:
		case <-timer.C:
			t.mu.Lock()
			if t.portForwardStopChan == myStopChan {
				t.portForwardStopChan = nil
			}
			t.mu.Unlock()
			if t.connector != nil {
				t.connector.SetLastError(fmt.Errorf("%w: ForwardPorts goroutine did not exit within %s", ErrPortForwardDied, monitorExitTimeout))
			}
			t.log.Error(nil, "port-forward goroutine did not exit after stop signal; abandoning monitor", "pod", podName, "timeout", monitorExitTimeout)
			return
		}
	}

	stderr := stderrBuf.String()

	// Capture state under t.mu, then call SetLastError without holding
	// t.mu to avoid nested lock ordering (t.mu -> c.mu).
	var notifyErr error
	t.mu.Lock()
	shouldNotify := t.portForwardStopChan == myStopChan
	if shouldNotify {
		t.portForwardStopChan = nil
		stderrSnippet := stderr
		if len(stderrSnippet) > maxLastErrorStderr {
			stderrSnippet = stderrSnippet[:maxLastErrorStderr] + "... [truncated]"
		}
		if err != nil {
			notifyErr = fmt.Errorf("%w: %v (stderr: %s)", ErrPortForwardDied, err, stderrSnippet)
		} else {
			notifyErr = ErrPortForwardDied
		}
	}
	t.mu.Unlock()
	if shouldNotify && t.connector != nil {
		t.connector.SetLastError(notifyErr)
	}

	if err != nil {
		t.log.Error(err, "port-forward died", "pod", podName, "stderr", stderr)
	} else {
		t.log.V(1).Info("port-forward closed", "pod", podName)
	}
}

func (t *tunnelStrategy) resolveRouterPod(ctx context.Context) (string, error) {
	if t.discoveryClient == nil {
		return "", fmt.Errorf("sandbox: discovery client required for port-forward")
	}
	selector := "kubernetes.io/service-name=sandbox-router-svc"
	slices, err := t.discoveryClient.EndpointSlices(t.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
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

// stopPortForward requires t.mu to be held.
func (t *tunnelStrategy) stopPortForward() {
	if t.portForwardStopChan != nil {
		close(t.portForwardStopChan)
		t.portForwardStopChan = nil
	}
	if t.pfDialer != nil {
		t.pfDialer.Close()
		t.pfDialer = nil
	}
	if t.spdyUpgradeClient != nil {
		t.spdyUpgradeClient.CloseIdleConnections()
		t.spdyUpgradeClient = nil
	}
}
