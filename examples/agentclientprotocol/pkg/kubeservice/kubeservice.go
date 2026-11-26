/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package kubeservice connects to a TCP port of a Kubernetes Service without
// exposing the Service outside the cluster. It resolves the Service to one of
// its backing pods and opens a port-forward stream to that pod through the
// API server, in-process: the same pods/portforward subresource that
// kubectl port-forward uses.
//
// No local listening port or kubectl subprocess is needed.
//
// Targets are written as URLs with a scheme of our own:
//
//	kubeservice://<context>/<namespace>/<service>[:<port>]
//
// <context> names a kubeconfig context, taken verbatim: context names are
// free-form (GKE's gke_<project>_<zone>_<cluster>, EKS's
// arn:aws:eks:<region>:<account>:cluster/<name>, user@cluster), so they are
// not parsed as a URL host. Leave it empty (kubeservice:///<namespace>/<service>)
// to use the current context. <port> is a Service port number or name and
// may be omitted when the Service has exactly one port. Only the Service's
// selector and port mapping are used, so a headless Service (clusterIP:
// None) is enough.
package kubeservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
)

// Scheme is the URL scheme of kubeservice targets.
const Scheme = "kubeservice"

// IsURL reports whether s is a kubeservice:// URL (as opposed to, say, a
// plain host:port address).
func IsURL(s string) bool {
	return strings.HasPrefix(s, Scheme+"://")
}

// Target identifies a Service port in a cluster; see ParseURL.
type Target struct {
	// Context is the kubeconfig context; empty means the current context.
	Context string
	// Namespace and Service name the Service.
	Namespace string
	Service   string
	// Port is the Service port number or name; empty means the Service's
	// only port.
	Port string
}

// ParseURL parses a kubeservice://<context>/<namespace>/<service>[:<port>]
// URL.
//
// The URL is not parsed with net/url: kubeconfig context names may contain
// ':' and '/' (EKS ARNs) or '@', which net/url rejects or misreads in a
// host. Instead it is split from the right: namespace and service are
// Kubernetes names, so they cannot contain '/', which makes the last two
// components unambiguous and leaves everything before them as the context.
func ParseURL(raw string) (*Target, error) {
	rest, ok := strings.CutPrefix(raw, Scheme+"://")
	if !ok {
		return nil, fmt.Errorf("%q: expected a %s:// URL", raw, Scheme)
	}
	formatErr := fmt.Errorf("%q: expected %s://<context>/<namespace>/<service>[:<port>]", raw, Scheme)

	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return nil, formatErr
	}
	j := strings.LastIndex(rest[:i], "/")
	if j < 0 {
		return nil, formatErr
	}
	kubeContext, namespace := rest[:j], rest[j+1:i]
	service, port, hasPort := strings.Cut(rest[i+1:], ":")

	if !isKubernetesName(namespace) {
		return nil, fmt.Errorf("%q: namespace %q must be lowercase alphanumeric or '-'", raw, namespace)
	}
	if !isKubernetesName(service) {
		return nil, fmt.Errorf("%q: service %q must be lowercase alphanumeric or '-'", raw, service)
	}
	// A colon with nothing after it is a mistake, not "the only port".
	if hasPort && !isKubernetesName(port) {
		return nil, fmt.Errorf("%q: port %q must be a port number or name", raw, port)
	}
	return &Target{Context: kubeContext, Namespace: namespace, Service: service, Port: port}, nil
}

// isKubernetesName reports whether s is a non-empty string of lowercase
// letters, digits and '-', the alphabet of namespace, Service and port names
// (a superset check: it does not enforce start/end characters or length).
func isKubernetesName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// String formats the target as a kubeservice:// URL; ParseURL(t.String())
// yields t again, whatever the context name.
func (t *Target) String() string {
	s := fmt.Sprintf("%s://%s/%s/%s", Scheme, t.Context, t.Namespace, t.Service)
	if t.Port != "" {
		s += ":" + t.Port
	}
	return s
}

// Dialer opens connections to Service ports in one cluster.
type Dialer struct {
	restConfig *rest.Config
	client     kubernetes.Interface
}

// NewDialer creates a Dialer for the named kubeconfig context (empty for the
// current context), loading the kubeconfig the way kubectl does
// ($KUBECONFIG or ~/.kube/config).
func NewDialer(kubeContext string) (*Dialer, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return &Dialer{restConfig: restConfig, client: client}, nil
}

// Dial connects to the target's Service port, using the target's kubeconfig
// context. It is a convenience for NewDialer followed by Dialer.Dial.
func Dial(ctx context.Context, target *Target) (*Conn, error) {
	dialer, err := NewDialer(target.Context)
	if err != nil {
		return nil, err
	}
	return dialer.Dial(ctx, target)
}

// Dial resolves the target's Service to a ready backing pod and the
// container port behind the Service port, and opens a port-forward stream
// to it. The target's Context is ignored: the Dialer already has a cluster.
func (d *Dialer) Dial(ctx context.Context, target *Target) (*Conn, error) {
	ep, err := d.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	conn, err := d.dialPod(ep)
	if err != nil {
		return nil, fmt.Errorf("port-forwarding to pod %s/%s port %d: %w", ep.namespace, ep.pod, ep.port, err)
	}
	return conn, nil
}

// endpoint is a pod port that backs a Service port.
type endpoint struct {
	namespace string
	pod       string
	port      int32
}

// resolve picks a pod and container port for the target's Service port, the
// way kubectl port-forward svc/<name> does: Service → selector → ready pod,
// and Service port → targetPort → container port.
func (d *Dialer) resolve(ctx context.Context, target *Target) (*endpoint, error) {
	svc, err := d.client.CoreV1().Services(target.Namespace).Get(ctx, target.Service, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting service %s/%s: %w", target.Namespace, target.Service, err)
	}
	svcPort, err := selectServicePort(svc, target.Port)
	if err != nil {
		return nil, fmt.Errorf("service %s/%s: %w", target.Namespace, target.Service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s/%s has no selector, so its backing pods cannot be found", target.Namespace, target.Service)
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector).String()
	pods, err := d.client.CoreV1().Pods(target.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing pods for service %s/%s (selector %s): %w", target.Namespace, target.Service, selector, err)
	}
	pod, err := pickPod(pods.Items)
	if err != nil {
		return nil, fmt.Errorf("service %s/%s (selector %s): %w", target.Namespace, target.Service, selector, err)
	}
	port, err := containerPort(pod, svcPort)
	if err != nil {
		return nil, fmt.Errorf("service %s/%s port %d: %w", target.Namespace, target.Service, svcPort.Port, err)
	}
	return &endpoint{namespace: pod.Namespace, pod: pod.Name, port: port}, nil
}

// selectServicePort returns the Service port named or numbered by want, or
// the Service's only port if want is empty.
func selectServicePort(svc *corev1.Service, want string) (*corev1.ServicePort, error) {
	ports := svc.Spec.Ports
	if want == "" {
		switch len(ports) {
		case 0:
			return nil, errors.New("service has no ports")
		case 1:
			return &ports[0], nil
		default:
			return nil, fmt.Errorf("service has %d ports, specify one with :<port> (%s)", len(ports), describePorts(ports))
		}
	}
	for i := range ports {
		p := &ports[i]
		if p.Name == want || strconv.Itoa(int(p.Port)) == want {
			return p, nil
		}
	}
	return nil, fmt.Errorf("service has no port %q (%s)", want, describePorts(ports))
}

func describePorts(ports []corev1.ServicePort) string {
	var out []string
	for _, p := range ports {
		if p.Name != "" {
			out = append(out, fmt.Sprintf("%s=%d", p.Name, p.Port))
		} else {
			out = append(out, strconv.Itoa(int(p.Port)))
		}
	}
	return strings.Join(out, ", ")
}

// pickPod returns a running, ready pod that is not being deleted, choosing
// deterministically (by name) when several qualify.
func pickPod(pods []corev1.Pod) (*corev1.Pod, error) {
	var ready []*corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = append(ready, pod)
				break
			}
		}
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("no ready pods (%d matching pods)", len(pods))
	}
	slices.SortFunc(ready, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return ready[0], nil
}

// containerPort resolves a Service port's targetPort to a port number on the
// pod: numeric targetPorts are used as-is, named ones are looked up in the
// pod's container ports.
func containerPort(pod *corev1.Pod, svcPort *corev1.ServicePort) (int32, error) {
	switch svcPort.TargetPort.Type {
	case intstr.Int:
		if n := svcPort.TargetPort.IntVal; n != 0 {
			return n, nil
		}
		// An unset targetPort defaults to the Service port.
		return svcPort.Port, nil
	case intstr.String:
		name := svcPort.TargetPort.StrVal
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == name {
					return p.ContainerPort, nil
				}
			}
		}
		return 0, fmt.Errorf("pod %s has no container port named %q", pod.Name, name)
	default:
		return 0, fmt.Errorf("unsupported targetPort %v", svcPort.TargetPort)
	}
}

// dialPod opens a port-forward stream to one pod port. This follows what
// kubectl port-forward does per accepted local connection, minus the local
// listener: negotiate a streaming connection (WebSocket, falling back to
// SPDY for API servers that do not support it), then open an error stream
// and a data stream for the port.
func (d *Dialer) dialPod(ep *endpoint) (*Conn, error) {
	u := d.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ep.namespace).
		Name(ep.pod).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(d.restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating SPDY round tripper: %w", err)
	}
	spdyDialer := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	websocketDialer, err := portforward.NewSPDYOverWebsocketDialerForStreaming(u, d.restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating WebSocket dialer: %w", err)
	}
	dialer := portforward.NewFallbackDialerForStreaming(websocketDialer, spdyDialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})

	streamConn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(int(ep.port)))
	// One forwarded connection per streaming connection, so the request ID
	// never needs to change.
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errorStream, err := streamConn.CreateStream(headers)
	if err != nil {
		streamConn.Close()
		return nil, fmt.Errorf("creating error stream: %w", err)
	}
	// The error stream is only read, never written.
	errorStream.Close()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := streamConn.CreateStream(headers)
	if err != nil {
		streamConn.Close()
		return nil, fmt.Errorf("creating data stream: %w", err)
	}

	conn := &Conn{
		Namespace:   ep.namespace,
		Pod:         ep.pod,
		Port:        ep.port,
		streamConn:  streamConn,
		dataStream:  dataStream,
		forwardDone: make(chan struct{}),
	}
	go conn.readErrorStream(errorStream)
	return conn, nil
}

// Conn is a forwarded connection to a pod port. It is an io.ReadWriteCloser
// carrying the bytes of the TCP connection the kubelet opened to the port.
type Conn struct {
	// Namespace, Pod and Port identify the pod port the connection reaches.
	Namespace string
	Pod       string
	Port      int32

	streamConn httpstream.Connection
	dataStream httpstream.Stream

	// forwardDone is closed once the kubelet's error stream ends;
	// forwardErr then holds the forwarding error it reported, if any (for
	// example a connection refused by the pod).
	forwardDone chan struct{}
	forwardErr  error

	closeOnce sync.Once
}

// readErrorStream collects what the kubelet reports on the error stream. The
// kubelet writes nothing on success and a message such as "error forwarding
// port ...: connection refused" on failure, then closes the stream once the
// forward ends.
func (c *Conn) readErrorStream(errorStream httpstream.Stream) {
	defer close(c.forwardDone)
	message, err := io.ReadAll(errorStream)
	switch {
	case err != nil:
		c.forwardErr = fmt.Errorf("reading port-forward error stream: %w", err)
	case len(message) > 0:
		c.forwardErr = errors.New(string(message))
	}
}

// Read reads from the forwarded connection. When the remote side closes,
// it waits briefly for the kubelet's error stream and reports the
// forwarding error there instead of a bare io.EOF, so a refused connection
// in the pod surfaces as such rather than as an abrupt disconnect.
func (c *Conn) Read(p []byte) (int, error) {
	n, err := c.dataStream.Read(p)
	if errors.Is(err, io.EOF) {
		select {
		case <-c.forwardDone:
			if c.forwardErr != nil {
				return n, c.forwardErr
			}
		case <-time.After(time.Second):
		}
	}
	return n, err
}

// Write writes to the forwarded connection.
func (c *Conn) Write(p []byte) (int, error) {
	return c.dataStream.Write(p)
}

// Close closes the forwarded connection and the underlying streaming
// connection to the API server.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.dataStream.Close()
		// Discard unsent data so a stalled peer cannot block the close.
		_ = c.dataStream.Reset()
		_ = c.streamConn.Close()
	})
	return nil
}
