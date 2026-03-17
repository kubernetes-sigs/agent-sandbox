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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const maxStderrSize = 64 << 10 // 64 KB

// syncBuffer is a goroutine-safe, size-capped bytes.Buffer for port-forward stderr.
// Write intentionally always reports len(p) written, even when truncating,
// to avoid breaking the port-forward library which may abort on write errors.
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
// can be forcefully closed when the stop channel alone does not unblock ForwardPorts
// (e.g., when the SPDY stream is stuck on a blocking read).
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

// Close forcefully closes the tracked SPDY connection, causing any blocking
// reads/writes on the connection to return errors and ForwardPorts to exit.
func (d *trackingDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// startPortForward establishes a port-forward tunnel to the sandbox-router service.
func (c *SandboxClient) startPortForward(ctx context.Context) (retErr error) {
	ctx, span := c.startSpan(ctx, "dev_mode_tunnel")
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	if c.coreClient == nil || c.restConfig == nil {
		return fmt.Errorf("sandbox: core client and REST config required for port-forward")
	}

	// Stop any existing port-forward to prevent goroutine leaks on reconnect.
	c.mu.Lock()
	c.stopPortForward()
	c.mu.Unlock()

	podName, err := c.resolveRouterPod(ctx)
	if err != nil {
		return err
	}

	reqURL := c.coreClient.RESTClient().Post().
		Resource("pods").
		Namespace(c.opts.Namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(c.restConfig)
	if err != nil {
		return fmt.Errorf("sandbox: failed to create SPDY round tripper: %w", err)
	}
	spdyClient := &http.Client{Transport: transport, Timeout: c.opts.PortForwardReadyTimeout}
	td := &trackingDialer{inner: spdy.NewDialer(upgrader, spdyClient, http.MethodPost, reqURL)}

	c.mu.Lock()
	if c.spdyUpgradeClient != nil {
		c.spdyUpgradeClient.CloseIdleConnections()
	}
	c.spdyUpgradeClient = spdyClient
	c.pfDialer = td
	c.mu.Unlock()

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	var stderrBuf syncBuffer
	fw, err := portforward.New(td, []string{"0:8080"}, stopChan, readyChan, io.Discard, &stderrBuf)
	if err != nil {
		return fmt.Errorf("sandbox: failed to create port forwarder: %w", err)
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

	ctx, cancel := context.WithTimeout(ctx, c.opts.PortForwardReadyTimeout)
	defer cancel()

	select {
	case <-readyChan:
		ports, err := fw.GetPorts()
		if err != nil {
			close(stopChan)
			td.Close()
			return fmt.Errorf("sandbox: failed to get forwarded ports: %w", err)
		}
		if len(ports) == 0 {
			close(stopChan)
			td.Close()
			return fmt.Errorf("sandbox: port forwarder reported no forwarded ports")
		}
		c.mu.Lock()
		c.baseURL = fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
		c.portForwardStopChan = stopChan
		c.mu.Unlock()
		c.log.Info("port-forward established", "localPort", ports[0].Local, "pod", podName)
		go c.monitorPortForward(errChan, &stderrBuf, podName, stopChan)
		return nil
	case err := <-errChan:
		if stderr := stderrBuf.String(); stderr != "" {
			return fmt.Errorf("sandbox: port-forward failed: %w (stderr: %s)", err, stderr)
		}
		return fmt.Errorf("sandbox: port-forward failed: %w", err)
	case <-ctx.Done():
		close(stopChan)
		td.Close() // Force-close SPDY connection to unblock ForwardPorts.
		go func() {
			timer := time.NewTimer(monitorExitTimeout)
			defer timer.Stop()
			select {
			case <-errChan:
			case <-timer.C:
				c.log.Error(nil, "port-forward goroutine did not exit after cancellation; resources may be leaked", "pod", podName, "timeout", monitorExitTimeout)
			}
		}()
		return fmt.Errorf("sandbox: port-forward cancelled: %w", ctx.Err())
	}
}

// monitorExitTimeout is how long the monitor waits for ForwardPorts to
// return after the stop channel is closed before abandoning the goroutine.
const monitorExitTimeout = 5 * time.Second

// maxLastErrorStderr caps stderr in the persistent lastError to avoid
// large error chains propagated through every subsequent ErrNotReady.
const maxLastErrorStderr = 256

// monitorPortForward clears baseURL when the port-forward exits so
// subsequent operations fail fast instead of timing out.
func (c *SandboxClient) monitorPortForward(errChan <-chan error, stderrBuf *syncBuffer, podName string, myStopChan chan struct{}) {
	var err error
	select {
	case err = <-errChan:
		// ForwardPorts returned (naturally or after stopChan closed).
	case <-myStopChan:
		// Stop requested; give ForwardPorts time to exit cleanly.
		timer := time.NewTimer(monitorExitTimeout)
		defer timer.Stop()
		select {
		case err = <-errChan:
		case <-timer.C:
			c.mu.Lock()
			if c.portForwardStopChan == myStopChan {
				c.portForwardStopChan = nil
				c.baseURL = ""
			}
			if c.lastError == nil {
				c.lastError = fmt.Errorf("%w: ForwardPorts goroutine did not exit within %s", ErrPortForwardDied, monitorExitTimeout)
			}
			c.mu.Unlock()
			c.log.Error(nil, "port-forward goroutine did not exit after stop signal; abandoning monitor", "pod", podName, "timeout", monitorExitTimeout)
			return
		}
	}

	stderr := stderrBuf.String()

	c.mu.Lock()
	if c.portForwardStopChan == myStopChan {
		c.portForwardStopChan = nil
		c.baseURL = ""
		stderrSnippet := stderr
		if len(stderrSnippet) > maxLastErrorStderr {
			stderrSnippet = stderrSnippet[:maxLastErrorStderr] + "... [truncated]"
		}
		if err != nil {
			c.lastError = fmt.Errorf("%w: %v (stderr: %s)", ErrPortForwardDied, err, stderrSnippet)
		} else {
			c.lastError = ErrPortForwardDied
		}
	}
	c.mu.Unlock()

	// Log the full stderr for diagnostics.
	if err != nil {
		c.log.Error(err, "port-forward died", "pod", podName, "stderr", stderr)
	} else {
		c.log.V(1).Info("port-forward closed", "pod", podName)
	}
}

// resolveRouterPod finds a pod backing the sandbox-router-svc service
// using the discoveryv1 EndpointSlice API.
func (c *SandboxClient) resolveRouterPod(ctx context.Context) (string, error) {
	if c.discoveryClient == nil {
		return "", fmt.Errorf("sandbox: discovery client required for port-forward")
	}
	selector := "kubernetes.io/service-name=sandbox-router-svc"
	slices, err := c.discoveryClient.EndpointSlices(c.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("sandbox: failed to list EndpointSlices for sandbox-router-svc: %w", err)
	}
	for _, slice := range slices.Items {
		for _, ep := range slice.Endpoints {
			if ep.TargetRef != nil && ep.TargetRef.Name != "" {
				return ep.TargetRef.Name, nil
			}
		}
	}
	return "", fmt.Errorf("sandbox: no ready endpoints for sandbox-router-svc in namespace %s", c.opts.Namespace)
}
