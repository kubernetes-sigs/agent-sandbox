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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forwardReadyTimeout bounds how long a port-forward may take to establish.
const forwardReadyTimeout = 30 * time.Second

// PodPortForwarder establishes apiserver-brokered SPDY port-forwards to
// sandbox pods, like kubectl port-forward. The Go SDK's own tunnel only
// carries the sandboxd data-plane ports, so the controller forwards the
// runner's HTTP port itself.
type PodPortForwarder struct {
	restCfg *rest.Config
}

// NewPodPortForwarder creates a forwarder from the controller's REST config.
func NewPodPortForwarder(restCfg *rest.Config) *PodPortForwarder {
	return &PodPortForwarder{restCfg: restCfg}
}

// Forward binds a free localhost port to namespace/podName:remotePort and
// returns the base URL of the local endpoint. The caller must invoke stop
// when done; stop is idempotent.
func (f *PodPortForwarder) Forward(ctx context.Context, namespace, podName string, remotePort int) (string, func(), error) {
	u, err := url.Parse(f.restCfg.Host)
	if err != nil {
		return "", nil, fmt.Errorf("parsing API server host %q: %w", f.restCfg.Host, err)
	}
	// The dialer targets the pod's portforward subresource directly, the same
	// way clients/go/sandbox builds its tunnel.
	u.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)

	transport, upgrader, err := spdy.RoundTripperFor(f.restCfg)
	if err != nil {
		return "", nil, fmt.Errorf("building SPDY round tripper: %w", err)
	}
	spdyClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	dialer := spdy.NewDialerForStreaming(upgrader, spdyClient, http.MethodPost, u)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	errCh := make(chan error, 1)
	var out, errOut bytes.Buffer

	fw, err := portforward.NewOnAddressesForStreaming(
		dialer,
		[]string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", remotePort)},
		stopCh, readyCh, &out, &errOut,
	)
	if err != nil {
		return "", nil, fmt.Errorf("creating port-forward to %s/%s: %w", namespace, podName, err)
	}
	go func() { errCh <- fw.ForwardPorts() }()

	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	select {
	case <-readyCh:
	case err := <-errCh:
		stop()
		return "", nil, fmt.Errorf("port-forward to %s failed: %w (%s)", podName, err, errOut.String())
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	case <-time.After(forwardReadyTimeout):
		stop()
		return "", nil, fmt.Errorf("port-forward to %s not ready after %s (%s)", podName, forwardReadyTimeout, errOut.String())
	}

	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		stop()
		return "", nil, fmt.Errorf("reading forwarded ports: %w", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)

	// A forward that dies after setup surfaces through errCh; drain it so
	// the ForwardPorts goroutine exits instead of leaking.
	go func() {
		select {
		case <-errCh:
		case <-stopCh:
		}
	}()

	return baseURL, stop, nil
}
