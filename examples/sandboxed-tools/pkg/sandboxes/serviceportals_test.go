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

package sandboxes

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestAddServicePortal(t *testing.T) {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: sandboxv1beta1.SandboxSpec{
			PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sandbox",
							Image: "debian:bookworm-slim",
						},
					},
				},
			},
		},
	}

	AddServicePortal(sb, ServicePortalConfig{})

	podSpec := &sb.Spec.PodTemplate.Spec

	// Verify HostAliases
	if len(podSpec.HostAliases) != 1 {
		t.Fatalf("expected 1 host alias, got %d", len(podSpec.HostAliases))
	}
	if podSpec.HostAliases[0].IP != "8.8.8.8" {
		t.Errorf("expected IP 8.8.8.8, got %s", podSpec.HostAliases[0].IP)
	}
	expectedHosts := map[string]bool{"gemini.backend": true, "github.backend": true}
	for _, host := range podSpec.HostAliases[0].Hostnames {
		if !expectedHosts[host] {
			t.Errorf("unexpected hostname: %s", host)
		}
	}

	// Verify InitContainers
	if len(podSpec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(podSpec.InitContainers))
	}
	initContainer := podSpec.InitContainers[0]
	if initContainer.Name != "init-iptables" {
		t.Errorf("expected init container name 'init-iptables', got '%s'", initContainer.Name)
	}
	if initContainer.Image != "init-iptables:latest" {
		t.Errorf("expected init container image 'init-iptables:latest', got '%s'", initContainer.Image)
	}
	if initContainer.SecurityContext == nil || initContainer.SecurityContext.Capabilities == nil {
		t.Fatalf("init container SecurityContext or Capabilities is nil")
	}
	if !slices.Contains(initContainer.SecurityContext.Capabilities.Add, "NET_ADMIN") {
		t.Errorf("init container missing NET_ADMIN capability")
	}

	// Verify Containers
	if len(podSpec.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(podSpec.Containers))
	}

	// Sandbox container
	sandboxContainer := podSpec.Containers[0]
	if sandboxContainer.Name != "sandbox" {
		t.Errorf("expected first container to be 'sandbox', got '%s'", sandboxContainer.Name)
	}
	if sandboxContainer.SecurityContext == nil {
		t.Fatalf("sandbox container SecurityContext is nil")
	}
	if sandboxContainer.SecurityContext.RunAsNonRoot == nil || !*sandboxContainer.SecurityContext.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot to be true")
	}
	if sandboxContainer.SecurityContext.RunAsUser == nil || *sandboxContainer.SecurityContext.RunAsUser != 1000 {
		t.Errorf("expected RunAsUser to be 1000")
	}
	if sandboxContainer.SecurityContext.AllowPrivilegeEscalation == nil || *sandboxContainer.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("expected AllowPrivilegeEscalation to be false")
	}

	// Sidecar container
	sidecarContainer := podSpec.Containers[1]
	if sidecarContainer.Name != "service-portal-sidecar" {
		t.Errorf("expected second container to be 'service-portal-sidecar', got '%s'", sidecarContainer.Name)
	}
	if sidecarContainer.Image != "all-in-one-portal:latest" {
		t.Errorf("expected sidecar container image 'all-in-one-portal:latest', got '%s'", sidecarContainer.Image)
	}
	if sidecarContainer.SecurityContext == nil {
		t.Fatalf("sidecar container SecurityContext is nil")
	}
	if sidecarContainer.SecurityContext.RunAsUser == nil || *sidecarContainer.SecurityContext.RunAsUser != 1337 {
		t.Errorf("expected sidecar RunAsUser to be 1337")
	}
	if sidecarContainer.SecurityContext.RunAsGroup == nil || *sidecarContainer.SecurityContext.RunAsGroup != 1337 {
		t.Errorf("expected sidecar RunAsGroup to be 1337")
	}
	if sidecarContainer.SecurityContext.AllowPrivilegeEscalation == nil || *sidecarContainer.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("expected sidecar AllowPrivilegeEscalation to be false")
	}
	if sidecarContainer.SecurityContext.ReadOnlyRootFilesystem == nil || !*sidecarContainer.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("expected sidecar ReadOnlyRootFilesystem to be true")
	}

	// Verify Env
	envMap := make(map[string]string)
	for _, env := range sidecarContainer.Env {
		if env.ValueFrom == nil {
			envMap[env.Name] = env.Value
		}
	}
	if envMap["SERVICE_NAMES"] != "gemini,github" {
		t.Errorf("expected SERVICE_NAMES env to be 'gemini,github', got '%s'", envMap["SERVICE_NAMES"])
	}
	if envMap["GEMINI_TARGET_URL"] != "https://generativelanguage.googleapis.com" {
		t.Errorf("expected GEMINI_TARGET_URL, got '%s'", envMap["GEMINI_TARGET_URL"])
	}
	if envMap["GEMINI_HOST"] != "gemini.backend" {
		t.Errorf("expected GEMINI_HOST, got '%s'", envMap["GEMINI_HOST"])
	}
	if envMap["GITHUB_TARGET_URL"] != "https://api.github.com" {
		t.Errorf("expected GITHUB_TARGET_URL, got '%s'", envMap["GITHUB_TARGET_URL"])
	}
	if envMap["GITHUB_HOST"] != "github.backend" {
		t.Errorf("expected GITHUB_HOST, got '%s'", envMap["GITHUB_HOST"])
	}
}
