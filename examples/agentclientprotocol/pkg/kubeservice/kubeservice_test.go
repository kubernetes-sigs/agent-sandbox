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

package kubeservice

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		url     string
		want    Target
		wantErr string
	}{
		{
			url:  "kubeservice://gke_proj_us-central1-b_cluster/codebot/sandboxed-tools-acp-server",
			want: Target{Context: "gke_proj_us-central1-b_cluster", Namespace: "codebot", Service: "sandboxed-tools-acp-server"},
		},
		{
			url:  "kubeservice://kind-agent-sandbox/default/acp:8090",
			want: Target{Context: "kind-agent-sandbox", Namespace: "default", Service: "acp", Port: "8090"},
		},
		{
			url:  "kubeservice://ctx/ns/svc:acp",
			want: Target{Context: "ctx", Namespace: "ns", Service: "svc", Port: "acp"},
		},
		{
			// Empty context selects the current kubeconfig context.
			url:  "kubeservice:///ns/svc",
			want: Target{Namespace: "ns", Service: "svc"},
		},
		{
			// Context names are free-form: EKS uses the cluster ARN, with
			// ':' and '/' that a URL host could not carry.
			url:  "kubeservice://arn:aws:eks:us-west-2:123456789012:cluster/prod/ns/svc:8090",
			want: Target{Context: "arn:aws:eks:us-west-2:123456789012:cluster/prod", Namespace: "ns", Service: "svc", Port: "8090"},
		},
		{
			url:  "kubeservice://admin@prod-cluster/ns/svc",
			want: Target{Context: "admin@prod-cluster", Namespace: "ns", Service: "svc"},
		},
		{url: "localhost:8090", wantErr: "expected a kubeservice:// URL"},
		{url: "http://ctx/ns/svc", wantErr: "expected a kubeservice:// URL"},
		{url: "kubeservice://ctx/svc", wantErr: "expected kubeservice://<context>/<namespace>/<service>"},
		{url: "kubeservice://ctx//svc", wantErr: `namespace ""`},
		{url: "kubeservice://ctx/ns/:8090", wantErr: `service ""`},
		{url: "kubeservice://ctx/ns/svc?port=1", wantErr: `service "svc?port=1"`},
		{url: "kubeservice://ctx/ns/svc:8090:1", wantErr: `port "8090:1"`},
		{url: "kubeservice://ctx/ns/svc:", wantErr: `port ""`},
		{url: "kubeservice://ctx/NS/svc", wantErr: `namespace "NS"`},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			got, err := ParseURL(tc.url)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseURL(%q) error = %v, want containing %q", tc.url, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q) unexpected error: %v", tc.url, err)
			}
			if *got != tc.want {
				t.Errorf("ParseURL(%q) = %+v, want %+v", tc.url, *got, tc.want)
			}
			if got.String() != tc.url {
				t.Errorf("Target.String() = %q, want round trip of %q", got.String(), tc.url)
			}
		})
	}
}

func TestSelectServicePort(t *testing.T) {
	one := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "acp", Port: 8090}}}}
	two := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "acp", Port: 8090}, {Name: "metrics", Port: 9090}}}}
	none := &corev1.Service{}

	tests := []struct {
		name     string
		svc      *corev1.Service
		want     string
		wantPort int32
		wantErr  string
	}{
		{name: "only port by default", svc: one, wantPort: 8090},
		{name: "by number", svc: two, want: "9090", wantPort: 9090},
		{name: "by name", svc: two, want: "metrics", wantPort: 9090},
		{name: "ambiguous", svc: two, wantErr: "specify one with :<port> (acp=8090, metrics=9090)"},
		{name: "unknown", svc: two, want: "http", wantErr: `no port "http"`},
		{name: "no ports", svc: none, wantErr: "no ports"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectServicePort(tc.svc, tc.want)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Port != tc.wantPort {
				t.Errorf("port = %d, want %d", got.Port, tc.wantPort)
			}
		})
	}
}

func pod(name string, phase corev1.PodPhase, ready bool, terminating bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"app": "acp"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "server",
			Ports: []corev1.ContainerPort{{Name: "acp", ContainerPort: 8090}},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestPickPod(t *testing.T) {
	pods := []corev1.Pod{
		pod("c-ready", corev1.PodRunning, true, false),
		pod("a-terminating", corev1.PodRunning, true, true),
		pod("b-ready", corev1.PodRunning, true, false),
		pod("a-notready", corev1.PodRunning, false, false),
		pod("a-pending", corev1.PodPending, false, false),
	}
	got, err := pickPod(pods)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "b-ready" {
		t.Errorf("picked %q, want the first ready pod by name, b-ready", got.Name)
	}

	if _, err := pickPod(pods[3:]); err == nil || !strings.Contains(err.Error(), "no ready pods (2 matching pods)") {
		t.Errorf("error = %v, want no ready pods", err)
	}
}

func TestContainerPort(t *testing.T) {
	p := pod("p", corev1.PodRunning, true, false)
	tests := []struct {
		name    string
		port    corev1.ServicePort
		want    int32
		wantErr string
	}{
		{name: "numeric targetPort", port: corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8090)}, want: 8090},
		{name: "unset targetPort defaults to port", port: corev1.ServicePort{Port: 8090}, want: 8090},
		{name: "named targetPort", port: corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("acp")}, want: 8090},
		{name: "unknown named targetPort", port: corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("grpc")}, wantErr: `no container port named "grpc"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containerPort(&p, &tc.port)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("port = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "acp", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{"app": "acp"},
			Ports:     []corev1.ServicePort{{Name: "acp", Port: 80, TargetPort: intstr.FromString("acp")}},
		},
	}
	noSelector := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	ready := pod("ready", corev1.PodRunning, true, false)
	other := pod("other-app", corev1.PodRunning, true, false)
	other.Labels = map[string]string{"app": "other"}

	d := &Dialer{client: fake.NewClientset(svc, noSelector, &ready, &other)}
	ctx := context.Background()

	ep, err := d.resolve(ctx, &Target{Namespace: "ns", Service: "acp"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ep.namespace != "ns" || ep.pod != "ready" || ep.port != 8090 {
		t.Errorf("resolve = %+v, want pod ns/ready port 8090", *ep)
	}

	for _, tc := range []struct {
		name    string
		target  Target
		wantErr string
	}{
		{name: "missing service", target: Target{Namespace: "ns", Service: "nope"}, wantErr: "not found"},
		{name: "no selector", target: Target{Namespace: "ns", Service: "external"}, wantErr: "has no selector"},
		{name: "wrong port", target: Target{Namespace: "ns", Service: "acp", Port: "81"}, wantErr: `no port "81"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.resolve(ctx, &tc.target)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
