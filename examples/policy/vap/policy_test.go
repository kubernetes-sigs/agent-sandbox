//go:build integration
// +build integration

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

package vap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

func TestSecureSandboxVAP(t *testing.T) {
	// 1. Setup EnvTest
	crdPath := filepath.Join("..", "..", "..", "k8s", "crds")

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer testEnv.Stop()

	// 2. Create Client
	scheme := runtime.NewScheme()
	_ = sandboxv1alpha1.AddToScheme(scheme)
	_ = admissionregistrationv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	// 3. Apply Policy
	if err := applyYAML(ctx, k8sClient, "secure-sandbox-policy.yaml"); err != nil {
		t.Fatalf("Failed to apply VAP: %v", err)
	}
	if err := applyYAML(ctx, k8sClient, "secure-sandbox-binding.yaml"); err != nil {
		t.Fatalf("Failed to apply VAP Binding: %v", err)
	}

	// Base secure spec to clone for failure cases
	secureSpec := corev1.PodSpec{
		RuntimeClassName:             ptr.To("gvisor"),
		HostNetwork:                  false,
		AutomountServiceAccountToken: ptr.To(false),
		NodeSelector:                 map[string]string{"sandbox.gke.io/runtime": "gvisor"},
		Tolerations: []corev1.Toleration{
			{Key: "sandbox.gke.io/runtime", Value: "gvisor", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
		},
		Containers: []corev1.Container{
			{
				Name:  "test",
				Image: "nginx",
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					ProcMount:    ptr.To(corev1.DefaultProcMount),
				},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    parserQuantity("100m"),
						corev1.ResourceMemory: parserQuantity("128Mi"),
					},
				},
			},
		},
	}

	// 4. Test Scenarios
	tests := []struct {
		name          string
		mutateSpec    func(spec *corev1.PodSpec) // Function to inject the vulnerability
		expectAllowed bool
	}{
		// --- 1. Success Case ---
		{
			name:          "Success: Secure Sandbox",
			mutateSpec:    func(spec *corev1.PodSpec) {}, // No changes
			expectAllowed: true,
		},

		// --- 2. Isolation Violations ---
		{
			name: "Violation: Runtime Class (Not gvisor)",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.RuntimeClassName = ptr.To("runc")
			},
			expectAllowed: false,
		},
		{
			name: "Violation: HostNetwork is True",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.HostNetwork = true
			},
			expectAllowed: false,
		},
		{
			name: "Violation: HostPID is True",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.HostPID = true
			},
			expectAllowed: false,
		},
		{
			name: "Violation: HostIPC is True",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.HostIPC = true
			},
			expectAllowed: false,
		},
		{
			name: "Violation: HostPort Used",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].Ports = []corev1.ContainerPort{
					{ContainerPort: 80, HostPort: 8080},
				}
			},
			expectAllowed: false,
		},

		// --- 3. Identity Violations ---
		{
			name: "Violation: Automount Service Account Token",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.AutomountServiceAccountToken = ptr.To(true)
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Projected Volume (ClusterTrustBundle / Pod Certs)",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Volumes = []corev1.Volume{
					{
						Name: "cert-vol",
						VolumeSource: corev1.VolumeSource{
							Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{
									{ClusterTrustBundle: &corev1.ClusterTrustBundleProjection{Name: ptr.To("my-cert")}},
								},
							},
						},
					},
				}
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Capabilities (Dropped NET_RAW instead of ALL)",
			mutateSpec: func(spec *corev1.PodSpec) {
				// We drop NET_RAW, but the policy strictly requires ALL now.
				spec.Containers[0].SecurityContext.Capabilities.Drop = []corev1.Capability{"NET_RAW"}
			},
			expectAllowed: false,
		},
		{
			name: "Success: Pod-level RunAsNonRoot is inherited",
			mutateSpec: func(spec *corev1.PodSpec) {
				// Remove container-level runAsNonRoot
				spec.Containers[0].SecurityContext.RunAsNonRoot = nil
				// Set it at the Pod level instead
				spec.SecurityContext = &corev1.PodSecurityContext{
					RunAsNonRoot: ptr.To(true),
				}
			},
			expectAllowed: true,
		},
		{
			name: "Violation: Container overrides Pod-level RunAsNonRoot to false",
			mutateSpec: func(spec *corev1.PodSpec) {
				// Pod says true
				spec.SecurityContext = &corev1.PodSecurityContext{
					RunAsNonRoot: ptr.To(true),
				}
				// Container maliciously overrides to false
				spec.Containers[0].SecurityContext.RunAsNonRoot = ptr.To(false)
			},
			expectAllowed: false,
		},

		// --- 4. Filesystem & Kernel Violations ---
		{
			name: "Violation: HostPath Volume",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Volumes = []corev1.Volume{
					{
						Name: "host-vol",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"},
						},
					},
				}
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Unmasked ProcMount",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].SecurityContext.ProcMount = ptr.To(corev1.UnmaskedProcMount)
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Sysctls Set",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.SecurityContext = &corev1.PodSecurityContext{
					Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_forward", Value: "1"}},
				}
			},
			expectAllowed: false,
		},

		// --- 5. Hardening Violations ---
		{
			name: "Violation: Privileged Container",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Capabilities (Didn't Drop)",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].SecurityContext.Capabilities.Drop = []corev1.Capability{}
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Capabilities (Added)",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
			},
			expectAllowed: false,
		},
		{
			name: "Violation: RunAsRoot",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].SecurityContext.RunAsNonRoot = ptr.To(false)
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Missing Resource Limits",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Containers[0].Resources.Limits = nil
			},
			expectAllowed: false,
		},

		// --- 6. Scheduling Violations ---
		{
			name: "Violation: Wrong Node Selector",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.NodeSelector = nil
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Missing Toleration",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Tolerations = nil
			},
			expectAllowed: false,
		},
		{
			name: "Violation: Projected Volume (PodCertificate)",
			mutateSpec: func(spec *corev1.PodSpec) {
				spec.Volumes = []corev1.Volume{
					{
						Name: "podcert-vol",
						VolumeSource: corev1.VolumeSource{
							Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{
									{
										// Blocks the exact attack vector Michael mentioned
										PodCertificate: &corev1.PodCertificateProjection{
											SignerName:           "coolcert.example.com/foo",
											CredentialBundlePath: "credentialbundle.pem",
										},
									},
								},
							},
						},
					},
				}
			},
			expectAllowed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// DeepCopy the secure spec to start fresh every time
			sandbox := &sandboxv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				Spec: sandboxv1alpha1.SandboxSpec{
					PodTemplate: sandboxv1alpha1.PodTemplate{
						Spec: *secureSpec.DeepCopy(),
					},
				},
			}

			// Apply the vulnerability
			tc.mutateSpec(&sandbox.Spec.PodTemplate.Spec)

			// Attempt Creation
			err := k8sClient.Create(ctx, sandbox)

			if tc.expectAllowed {
				if err != nil {
					t.Fatalf("Expected Sandbox to be created, but got error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("Security Fail: Expected VAP to reject insecure Sandbox, but it was allowed!")
				}
				// Cleanup failed sandbox (not strictly needed since Create failed, but good practice for allowed ones)
			}
			// Cleanup allowed sandboxes
			if err == nil {
				_ = k8sClient.Delete(ctx, sandbox)
			}
		})
	}
}

// Helper to apply YAML files
func applyYAML(ctx context.Context, c client.Client, filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	decoder := yaml.NewYAMLOrJSONDecoder(os.NewFile(0, filename), 4096)
	_ = decoder

	obj := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if filename == "secure-sandbox-binding.yaml" {
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		if err := yaml.Unmarshal(data, binding); err != nil {
			return err
		}
		binding.ResourceVersion = ""
		return c.Create(ctx, binding)
	}

	if err := yaml.Unmarshal(data, obj); err != nil {
		return err
	}
	obj.ResourceVersion = ""
	return c.Create(ctx, obj)
}

// Helper for resource quantities
func parserQuantity(q string) resource.Quantity {
	val, _ := resource.ParseQuantity(q)
	return val
}
