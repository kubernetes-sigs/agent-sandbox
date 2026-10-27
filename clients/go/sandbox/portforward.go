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
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
)

const (
	monitorExitTimeout = 5 * time.Second
	maxLastErrorStderr = 256
)

// portForwarder is the subset of client-go's portforward.PortForwarder that
// portForwardTunnel depends on. It is also the seam tests fake to drive start
// without a live apiserver.
type portForwarder interface {
	ForwardPorts() error
	GetPorts() ([]portforward.ForwardedPort, error)
}

// portForwardTunnel owns the lifecycle of a port-forward connection shared
// by strategies that target either the router or a sandbox pod.
type portForwardTunnel struct {
	name                string
	coreClient          corev1client.CoreV1Interface
	restConfig          *rest.Config
	namespace           string
	pfTimeout           time.Duration
	mu                  sync.Mutex
	portForwardStopChan chan struct{}
	spdyUpgradeClient   *http.Client
	pfDialer            *trackingDialer

	log    logr.Logger
	onDied func(error)

	// dial and newForwarder are test seams; when nil, start falls back to
	// the real client-go implementations (newDialer / NewForStreaming).
	dial         func(string) (*trackingDialer, error)
	newForwarder func(httpstream.Dialer, []string, <-chan struct{}, chan struct{}, io.Writer, io.Writer) (portForwarder, error)
}

// stopPortForward requires t.mu to be held.
func (t *portForwardTunnel) stopPortForward() {
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

// stop tears down any in-flight port-forward. Safe to call concurrently.
func (t *portForwardTunnel) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopPortForward()
}

func (t *portForwardTunnel) Close() error {
	t.stop()
	return nil
}

func (t *portForwardTunnel) monitorPortForward(errChan <-chan error, stderrBuf *syncBuffer, podName string, myStopChan chan struct{}) {
	var err error
	select {
	case err = <-errChan:
	case <-myStopChan:
		timer := time.NewTimer(monitorExitTimeout)
		defer timer.Stop()
		select {
		case err = <-errChan:
		case <-timer.C:
			// As below: a reconnect may have replaced this tunnel, and
			// onDied would tear down the replacement.
			t.mu.Lock()
			shouldNotify := t.portForwardStopChan == myStopChan
			if shouldNotify {
				t.portForwardStopChan = nil
			}
			t.mu.Unlock()
			if shouldNotify && t.onDied != nil {
				t.onDied(fmt.Errorf(
					"%w: ForwardPorts goroutine did not exit within %s",
					ErrPortForwardDied, monitorExitTimeout,
				))
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
	// Call onDied without holding t.mu to avoid nested lock ordering.
	if shouldNotify && t.onDied != nil {
		t.onDied(notifyErr)
	}

	if err != nil {
		t.log.Error(err, t.name+" died", "pod", podName, "stderr", stderr)
	} else {
		t.log.V(1).Info(t.name+" closed", "pod", podName)
	}
}

func (t *portForwardTunnel) newDialer(podName string) (*trackingDialer, error) {
	reqURL := t.coreClient.RESTClient().Post().
		Resource("pods").
		Namespace(t.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(t.restConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox: failed to create SPDY round tripper: %w", err)
	}

	spdyClient := &http.Client{
		Transport: transport,
		Timeout:   t.pfTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	td := &trackingDialer{
		inner: spdy.NewDialerForStreaming(
			upgrader, spdyClient, http.MethodPost, reqURL,
		),
	}

	t.mu.Lock()
	if t.spdyUpgradeClient != nil {
		t.spdyUpgradeClient.CloseIdleConnections()
	}
	t.spdyUpgradeClient = spdyClient
	t.pfDialer = td
	t.mu.Unlock()

	return td, nil
}

func runPortForward(fw portForwarder) <-chan error {
	errChan := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errChan <- fmt.Errorf("sandbox: port-forward panicked: %v", r)
			}
		}()
		errChan <- fw.ForwardPorts()
	}()
	return errChan
}

func (t *portForwardTunnel) start(
	ctx context.Context,
	podName string,
	forwardSpecs []string,
	errorLabel string,
	onReady func([]portforward.ForwardedPort) error,
) error {
	dial := t.dial
	if dial == nil {
		dial = t.newDialer
	}
	td, err := dial(podName)
	if err != nil {
		return err
	}

	newForwarder := t.newForwarder
	if newForwarder == nil {
		newForwarder = func(d httpstream.Dialer, specs []string, stop <-chan struct{}, ready chan struct{}, out io.Writer, errOut io.Writer) (portForwarder, error) {
			return portforward.NewForStreaming(d, specs, stop, ready, out, errOut)
		}
	}

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	var stderrBuf syncBuffer

	fw, err := newForwarder(td, forwardSpecs, stopChan, readyChan, io.Discard, &stderrBuf)
	if err != nil {
		return fmt.Errorf("sandbox: failed to create port forwarder: %w", err)
	}

	errChan := runPortForward(fw)

	pfCtx, pfCancel := context.WithTimeout(ctx, t.pfTimeout)
	defer pfCancel()

	stop := func() {
		close(stopChan)
		td.Close()
	}

	select {
	case <-readyChan:
		ports, err := fw.GetPorts()
		if err != nil {
			stop()
			return fmt.Errorf("sandbox: failed to get forwarded ports: %w", err)
		}

		if err := onReady(ports); err != nil {
			stop()
			return err
		}

		t.mu.Lock()
		t.portForwardStopChan = stopChan
		t.mu.Unlock()

		go t.monitorPortForward(errChan, &stderrBuf, podName, stopChan)
		return nil

	case err := <-errChan:
		td.Close()
		if stderr := stderrBuf.String(); stderr != "" {
			return fmt.Errorf(
				"sandbox: %s failed: %w (stderr: %s)",
				errorLabel, err, stderr,
			)
		}
		return fmt.Errorf("sandbox: %s failed: %w", errorLabel, err)

	case <-pfCtx.Done():
		stop()
		go func() {
			timer := time.NewTimer(monitorExitTimeout)
			defer timer.Stop()
			select {
			case err := <-errChan:
				t.log.V(4).Info("port-forward monitor exited after cancellation",
					"pod", podName, "err", err)
			case <-timer.C:
				t.log.Error(
					nil,
					"port-forward goroutine did not exit after cancellation; resources may be leaked",
					"pod", podName,
					"timeout", monitorExitTimeout,
				)
			}
		}()
		return fmt.Errorf("sandbox: %s cancelled: %w", errorLabel, pfCtx.Err())
	}
}
