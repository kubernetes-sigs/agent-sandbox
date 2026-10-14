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
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
)

// fakeForwarder implements portForwarder so start() can be driven through its
// ready, immediate-error, and timeout branches without a live apiserver.
type fakeForwarder struct {
	ports       []portforward.ForwardedPort
	getPortsErr error
	forwardErr  error

	// signalReady makes ForwardPorts close readyChan (like the real forwarder
	// does once it has bound its listeners) before blocking.
	signalReady bool

	stopChan  <-chan struct{}
	readyChan chan struct{}
	errOut    io.Writer

	// returned is closed once ForwardPorts has returned, so tests can wait for
	// the forward goroutine to finish before asserting no leaks.
	returned chan struct{}
}

func (f *fakeForwarder) ForwardPorts() error {
	defer close(f.returned)
	if f.signalReady {
		close(f.readyChan)
	}
	if f.forwardErr != nil {
		if f.errOut != nil {
			_, _ = io.WriteString(f.errOut, "boom: simulated forward failure")
		}
		return f.forwardErr
	}
	<-f.stopChan
	return nil
}

func (f *fakeForwarder) GetPorts() ([]portforward.ForwardedPort, error) {
	if f.getPortsErr != nil {
		return nil, f.getPortsErr
	}
	return f.ports, nil
}

// newTestPortForwardTunnel wires fw into a portForwardTunnel whose dialer and
// forwarder factory are stubbed, so start() exercises only its own lifecycle.
func newTestPortForwardTunnel(fw *fakeForwarder) *portForwardTunnel {
	return &portForwardTunnel{
		log:       logr.Discard(),
		pfTimeout: 5 * time.Second,
		dial: func(string) (*trackingDialer, error) {
			return &trackingDialer{}, nil
		},
		newForwarder: func(_ httpstream.Dialer, _ []string, stopChan <-chan struct{}, readyChan chan struct{}, _ io.Writer, errOut io.Writer) (portForwarder, error) {
			fw.stopChan = stopChan
			fw.readyChan = readyChan
			fw.errOut = errOut
			return fw, nil
		},
	}
}

func waitForwardReturned(t *testing.T, fw *fakeForwarder) {
	t.Helper()
	select {
	case <-fw.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("ForwardPorts did not return")
	}
}

func TestPortForwardTunnelStart_Ready(t *testing.T) {
	fw := &fakeForwarder{
		ports: []portforward.ForwardedPort{
			{Local: 12345, Remote: 8080},
			{Local: 12346, Remote: 9090},
		},
		signalReady: true,
		returned:    make(chan struct{}),
	}
	ts := newTestPortForwardTunnel(fw)

	var gotPorts []portforward.ForwardedPort
	var onReadyCalled bool
	err := ts.start(context.Background(), "test-pod", []string{"0:8080", "0:9090"}, "pod port-forward",
		func(ports []portforward.ForwardedPort) error {
			onReadyCalled = true
			gotPorts = ports
			return nil
		})
	require.NoError(t, err)
	require.True(t, onReadyCalled, "onReady should be called once the forwarder is ready")
	require.Equal(t, fw.ports, gotPorts)

	// The stop channel must be recorded so a later stop/Close tears down the
	// monitor goroutine and the forward connection.
	ts.mu.Lock()
	require.NotNil(t, ts.portForwardStopChan)
	ts.mu.Unlock()

	require.NoError(t, ts.Close())
	waitForwardReturned(t, fw)
}

func TestPortForwardTunnelStart_ForwardPortsError(t *testing.T) {
	fw := &fakeForwarder{
		forwardErr: errors.New("connection refused"),
		returned:   make(chan struct{}),
	}
	ts := newTestPortForwardTunnel(fw)

	err := ts.start(context.Background(), "test-pod", []string{"0:8080"}, "pod port-forward",
		func([]portforward.ForwardedPort) error {
			t.Fatal("onReady must not be called when forwarding fails immediately")
			return nil
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pod port-forward")
	require.Contains(t, err.Error(), "connection refused")
	require.Contains(t, err.Error(), "boom: simulated forward failure", "stderr should be surfaced")
	waitForwardReturned(t, fw)
}

func TestPortForwardTunnelStart_GetPortsError(t *testing.T) {
	fw := &fakeForwarder{
		signalReady: true,
		getPortsErr: errors.New("no ports bound"),
		returned:    make(chan struct{}),
	}
	ts := newTestPortForwardTunnel(fw)

	err := ts.start(context.Background(), "test-pod", []string{"0:8080"}, "pod port-forward",
		func([]portforward.ForwardedPort) error {
			t.Fatal("onReady must not be called when GetPorts fails")
			return nil
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no ports bound")
	// start() must stop the forwarder on GetPorts failure, unblocking
	// ForwardPorts.
	waitForwardReturned(t, fw)
}

func TestPortForwardTunnelStart_OnReadyError(t *testing.T) {
	fw := &fakeForwarder{
		signalReady: true,
		ports:       []portforward.ForwardedPort{{Local: 12345, Remote: 8080}},
		returned:    make(chan struct{}),
	}
	ts := newTestPortForwardTunnel(fw)

	onReadyErr := errors.New("missing grpc port")
	err := ts.start(context.Background(), "test-pod", []string{"0:8080"}, "pod port-forward",
		func([]portforward.ForwardedPort) error { return onReadyErr })
	require.ErrorIs(t, err, onReadyErr)
	waitForwardReturned(t, fw)
}

func TestPortForwardTunnelStart_Timeout(t *testing.T) {
	fw := &fakeForwarder{
		returned: make(chan struct{}),
	}
	ts := newTestPortForwardTunnel(fw)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ts.start(ctx, "test-pod", []string{"0:8080"}, "pod port-forward",
		func([]portforward.ForwardedPort) error {
			t.Fatal("onReady must not be called when the context is cancelled")
			return nil
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cancelled")
	// start() must stop the forwarder on cancellation, unblocking
	// ForwardPorts so the background goroutine can exit.
	waitForwardReturned(t, fw)
}
