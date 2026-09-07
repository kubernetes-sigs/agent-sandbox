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
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// podTunnelStrategy establishes a SPDY port-forward directly to the sandbox
// pod, carrying both sandboxd listeners: the Filesystem & Runtime REST API
// and the gRPC ProcessService. This is the only external transport that can
// reach sandboxd, which binds loopback-only inside the pod (KEP-539.2) —
// port-forward dials from within the pod's network namespace, whereas the
// sandbox-router dials the pod IP and is HTTP/1.1-only besides.
//
// Structure mirrors tunnelStrategy (tunnel.go), which forwards to the
// sandbox-router service for the legacy runtime instead.
type podTunnelStrategy struct {
	coreClient corev1client.CoreV1Interface
	restConfig *rest.Config
	namespace  string
	restPort   int
	grpcPort   int
	pfTimeout  time.Duration
	log        logr.Logger
	tracer     trace.Tracer
	svcName    string

	// getPodName returns the resolved sandbox pod name; set after
	// construction (the pod is only known once the sandbox is ready).
	getPodName func() string

	// Runtime state.
	portForwardStopChan chan struct{}
	spdyUpgradeClient   *http.Client
	pfDialer            *trackingDialer
	mu                  sync.Mutex

	// connector is set after construction so the monitor can signal death
	// and Connect can publish the forwarded gRPC target.
	connector *connector
}

func (t *podTunnelStrategy) Connect(ctx context.Context) (string, error) {
	ctx, span := startSpan(ctx, t.tracer, t.svcName, "sandboxd_pod_tunnel")
	defer span.End()

	if t.coreClient == nil || t.restConfig == nil {
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
	t.mu.Lock()
	t.stopPortForward()
	t.mu.Unlock()

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
	td := &trackingDialer{inner: spdy.NewDialerForStreaming(upgrader, spdyClient, http.MethodPost, reqURL)}

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
	forwardSpecs := []string{
		fmt.Sprintf("0:%d", t.restPort),
		fmt.Sprintf("0:%d", t.grpcPort),
	}
	fw, err := portforward.NewForStreaming(td, forwardSpecs, stopChan, readyChan, io.Discard, &stderrBuf)
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
			close(stopChan)
			td.Close()
			err := fmt.Errorf("sandbox: port forwarder did not report both sandboxd ports (rest=%d grpc=%d)", restLocal, grpcLocal)
			recordError(span, err)
			return "", err
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", restLocal)
		grpcTarget := fmt.Sprintf("127.0.0.1:%d", grpcLocal)
		if t.connector != nil {
			t.connector.SetGRPCTarget(grpcTarget)
		}
		t.mu.Lock()
		t.portForwardStopChan = stopChan
		t.mu.Unlock()
		t.log.Info("sandboxd pod port-forward established",
			"pod", podName, "restLocalPort", restLocal, "grpcLocalPort", grpcLocal)
		go t.monitorPortForward(errChan, &stderrBuf, podName, stopChan)
		return baseURL, nil
	case err := <-errChan:
		td.Close() // release SPDY connection established during Dial
		if stderr := stderrBuf.String(); stderr != "" {
			retErr := fmt.Errorf("sandbox: pod port-forward failed: %w (stderr: %s)", err, stderr)
			recordError(span, retErr)
			return "", retErr
		}
		recordError(span, err)
		return "", fmt.Errorf("sandbox: pod port-forward failed: %w", err)
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
		retErr := fmt.Errorf("sandbox: pod port-forward cancelled: %w", pfCtx.Err())
		recordError(span, retErr)
		return "", retErr
	}
}

func (t *podTunnelStrategy) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopPortForward()
	return nil
}

func (t *podTunnelStrategy) monitorPortForward(errChan <-chan error, stderrBuf *syncBuffer, podName string, myStopChan chan struct{}) {
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
		t.log.Error(err, "sandboxd pod port-forward died", "pod", podName, "stderr", stderr)
	} else {
		t.log.V(1).Info("sandboxd pod port-forward closed", "pod", podName)
	}
}

// stopPortForward requires t.mu to be held.
func (t *podTunnelStrategy) stopPortForward() {
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
