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

package agtsbx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// sandboxContainerName names the container in the generated pod template.
const sandboxContainerName = "sandbox"

// kubeBackend runs the sandbox as a core Sandbox object in a cluster.
//
// It uses the core Sandbox API rather than the SandboxClaim extension because
// `agtsbx run` names an image, and only Sandbox accepts one directly.
type kubeBackend struct {
	helper       *sandbox.K8sHelper
	namespace    string
	readyTimeout time.Duration
	httpClient   *http.Client
	progress     writerFunc
}

func newKubeBackend(opts backendOptions) (Backend, error) {
	// The SDK logs at a level suited to a long-lived service, which would
	// drown a one-shot command's output.
	helper, err := sandbox.NewK8sHelper(nil, logr.Discard())
	if err != nil {
		return nil, fmt.Errorf("connecting to Kubernetes (is your kubeconfig valid?): %w", err)
	}
	return &kubeBackend{
		helper:       helper,
		namespace:    opts.Namespace,
		readyTimeout: opts.ReadyTimeout,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		progress:     opts.Stderr,
	}, nil
}

func (b *kubeBackend) Name() string { return RuntimeKubernetes }

// kubeInstance is a started Kubernetes sandbox plus the port-forward that
// makes it reachable from the local machine.
type kubeInstance struct {
	backend   *kubeBackend
	name      string
	endpoints Endpoints
	remove    bool
	// stopForward is non-nil once Start returns successfully.
	stopForward func()
}

func (i *kubeInstance) Endpoints() Endpoints { return i.endpoints }

func (i *kubeInstance) Stop(ctx context.Context) error {
	i.stopForward()
	if !i.remove {
		return nil
	}
	if err := i.backend.delete(ctx, i.name); err != nil {
		return fmt.Errorf("deleting sandbox %s/%s: %w", i.backend.namespace, i.name, err)
	}
	return nil
}

// Start creates the Sandbox, waits for it to become Ready, then port-forwards
// to sandboxd inside its pod.
func (b *kubeBackend) Start(ctx context.Context, spec Spec) (Instance, error) {
	// One deadline for the whole startup: giving each wait below its own
	// would let Start take a multiple of the requested --timeout.
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()

	client := b.helper.AgentsClient.Sandboxes(b.namespace)
	if _, err := client.Create(ctx, buildSandbox(spec, b.namespace), metav1.CreateOptions{}); err != nil {
		// A deadline or connection error can hide a create the API server
		// already persisted, which would then run unattended. AlreadyExists
		// is somebody else's object, so leave that one alone.
		if !k8serrors.IsAlreadyExists(err) {
			b.deleteIfOurs(spec.Name)
		}
		return nil, fmt.Errorf("creating sandbox %s/%s: %w", b.namespace, spec.Name, err)
	}

	instance := &kubeInstance{backend: b, name: spec.Name, remove: spec.Remove}

	// The Sandbox now exists; unwind it on every failure below so a failed
	// run does not leave a pod consuming cluster capacity.
	if err := b.waitReady(ctx, spec.Name); err != nil {
		b.cleanup(instance.name)
		return nil, err
	}

	// The controller names the backing pod after the Sandbox, so the
	// port-forward target needs no lookup.
	endpoints, stop, err := b.forward(ctx, spec)
	if err != nil {
		b.cleanup(instance.name)
		return nil, err
	}
	instance.endpoints = endpoints
	instance.stopForward = stop

	// Ready only means the kubelet's probes passed. Confirm sandboxd answers
	// over the forwarded connection too, so a failure here is reported by
	// Start rather than by the first command.
	if err := waitHealthy(ctx, b.httpClient, endpoints.RESTAddr); err != nil {
		stop()
		b.cleanup(instance.name)
		return nil, err
	}
	return instance, nil
}

// delete removes the Sandbox. Foreground propagation makes the API server
// delete the backing pod before the Sandbox disappears, so teardown cannot be
// left to background garbage collection.
func (b *kubeBackend) delete(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationForeground
	err := b.helper.AgentsClient.Sandboxes(b.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	// A sandbox that is already gone is the state we wanted.
	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	return nil
}

// cleanup deletes a Sandbox created by a Start that then failed.
func (b *kubeBackend) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := b.delete(ctx, name); err != nil {
		fmt.Fprintf(b.progress, "agtsbx: warning: could not delete sandbox %s/%s: %v\n", b.namespace, name, err)
	}
}

// deleteIfOurs cleans up after an ambiguous create, but only if the object
// carries our label, so a Sandbox that happens to share the name survives.
func (b *kubeBackend) deleteIfOurs(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	existing, err := b.helper.AgentsClient.Sandboxes(b.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || existing.Labels[managedByLabel] != managedByValue {
		return
	}
	b.cleanup(name)
}

// waitReady blocks until the Sandbox reports the Ready condition. It watches
// rather than polls so a sandbox that starts quickly is not held up by a fixed
// poll interval.
func (b *kubeBackend) waitReady(ctx context.Context, name string) error {
	// Capped by any deadline the caller already set, so this shares one
	// startup budget with the health wait rather than taking a second one.
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()

	client := b.helper.AgentsClient.Sandboxes(b.namespace)

	// Check the current state first: with a warm image the Sandbox can go
	// Ready before the watch below is established, missing that event.
	current, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading sandbox %s/%s: %w", b.namespace, name, err)
	}
	if sandboxReady(current) {
		return nil
	}

	watcher, err := client.Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: current.ResourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watching sandbox %s/%s: %w", b.namespace, name, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s/%s did not become ready within %s: %w", b.namespace, name, b.readyTimeout, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch on sandbox %s/%s closed before it became ready", b.namespace, name)
			}
			switch event.Type {
			case watch.Deleted:
				return fmt.Errorf("sandbox %s/%s was deleted before it became ready", b.namespace, name)
			case watch.Error:
				// Spinning until the deadline would hide the real cause.
				return fmt.Errorf("watch on sandbox %s/%s failed: %w", b.namespace, name, k8serrors.FromObject(event.Object))
			}
			observed, ok := event.Object.(*sandboxv1beta1.Sandbox)
			if !ok {
				continue
			}
			if sandboxReady(observed) {
				return nil
			}
		}
	}
}

// sandboxReady reports whether the Sandbox's Ready condition is True.
func sandboxReady(sb *sandboxv1beta1.Sandbox) bool {
	for _, condition := range sb.Status.Conditions {
		if condition.Type == string(sandboxv1beta1.SandboxConditionReady) {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}

// forward establishes a port-forward carrying both sandboxd listeners and
// returns the local addresses plus a teardown function. Port-forward needs
// nothing installed in the cluster: no Service, Gateway or sandbox-router.
func (b *kubeBackend) forward(ctx context.Context, spec Spec) (Endpoints, func(), error) {
	reqURL := b.helper.CoreClient.RESTClient().Post().
		Resource("pods").
		Namespace(b.namespace).
		Name(spec.Name).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(b.helper.RestConfig)
	if err != nil {
		return Endpoints{}, nil, fmt.Errorf("creating SPDY round tripper: %w", err)
	}
	dialer := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})
	// Local port 0 lets the forwarder pick a free port, avoiding a
	// bind-then-release race between concurrent invocations.
	forwardSpecs := []string{
		fmt.Sprintf("0:%d", spec.RESTPort),
		fmt.Sprintf("0:%d", spec.GRPCPort),
	}
	forwarder, err := portforward.NewForStreaming(dialer, forwardSpecs, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return Endpoints{}, nil, fmt.Errorf("creating port forwarder: %w", err)
	}

	// Idempotent: error paths call stop and Stop may call it again.
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			close(stopChan)
		}
	}

	errChan := make(chan error, 1)
	go func() { errChan <- forwarder.ForwardPorts() }()

	select {
	case <-readyChan:
	case err := <-errChan:
		return Endpoints{}, nil, fmt.Errorf("port-forward to pod %s/%s failed: %w", b.namespace, spec.Name, err)
	case <-ctx.Done():
		stop()
		return Endpoints{}, nil, fmt.Errorf("port-forward to pod %s/%s cancelled: %w", b.namespace, spec.Name, ctx.Err())
	}

	ports, err := forwarder.GetPorts()
	if err != nil {
		stop()
		return Endpoints{}, nil, fmt.Errorf("reading forwarded ports: %w", err)
	}

	var restLocal, grpcLocal uint16
	for _, port := range ports {
		switch int(port.Remote) {
		case spec.RESTPort:
			restLocal = port.Local
		case spec.GRPCPort:
			grpcLocal = port.Local
		}
	}
	if restLocal == 0 || grpcLocal == 0 {
		stop()
		return Endpoints{}, nil, fmt.Errorf("port forwarder did not report both sandboxd ports (rest=%d grpc=%d)", restLocal, grpcLocal)
	}

	return Endpoints{
		GRPCAddr: fmt.Sprintf("127.0.0.1:%d", grpcLocal),
		RESTAddr: fmt.Sprintf("127.0.0.1:%d", restLocal),
	}, stop, nil
}

// buildSandbox renders spec into a Sandbox object. The pod template mirrors
// examples/sandboxd-sandbox/sandbox-template.yaml, so a sandbox behaves the
// same whether agtsbx started it or it was applied from a manifest.
//
// The command's environment is deliberately absent: it travels with the
// request instead, so credentials are not persisted in the object for anyone
// with get on sandboxes to read.
func buildSandbox(spec Spec, namespace string) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						AutomountServiceAccountToken: new(false),
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot: new(true),
						},
						Containers: []corev1.Container{{
							Name:  sandboxContainerName,
							Image: spec.Image,
							Ports: []corev1.ContainerPort{
								{Name: "rest", ContainerPort: int32(spec.RESTPort)},
								{Name: "grpc", ContainerPort: int32(spec.GRPCPort)},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: new(false),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
							// Without this the Sandbox reports Ready as soon
							// as the pod runs, before sandboxd accepts
							// requests.
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/v1/health",
										Port: intstr.FromInt32(int32(spec.RESTPort)),
									},
								},
							},
						}},
					},
				},
			},
		},
	}
}
