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

package pool

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func intstrFromInt32(p int32) intstr.IntOrString { return intstr.FromInt32(p) }

// PodBuilder constructs the bare Pod and backing PVC for a pool pod.
//
// A pool pod runs the pod-agent (the moat agentsandbox binary), mounts the
// base OCI rootfs read-only at /sandbox-image via Kubernetes image volume
// source, and mounts a shared PVC at /var/lib/sandboxes for per-tenant
// overlay uppers. Tenants are created and torn down dynamically via the
// pod-agent's gRPC API; the Pod itself is long-lived.
type PodBuilder struct {
	// AgentImage is the controller-side image containing the pod-agent
	// binary. Operators override this via a controller flag.
	AgentImage string

	// AgentPort is the TCP port the pod-agent gRPC server listens on.
	// Defaults to 7443.
	AgentPort int32

	// ProxyPort is the TCP port the pod-agent's HTTP reverse proxy listens
	// on (for HTTPRoute backend). Defaults to 8080.
	ProxyPort int32

	// Capacity is the number of bubblewrap tenants per pool pod. Defaults
	// to DefaultCapacity.
	Capacity int

	// PVCStorage is the requested size of the shared per-pool PVC.
	// Defaults to 10Gi.
	PVCStorage resource.Quantity

	// PVCStorageClass is the storage class for the per-pool PVC. Empty
	// means cluster default.
	PVCStorageClass string
}

// BuildPVC returns the PVC for a pool whose base image hashes to `hash`.
// The PVC carries a GenerateName so the API server picks a unique suffix
// at create time; the caller reads back the assigned `.Name` and uses it
// as the matching Pod (and Service) name. PVC, Pod and Service all share
// the same name to make grep-debugging painless and to keep the binding
// trivial: PVC name == Pod name == Pod's ClaimName.
//
// Generating the PVC first (not the Pod) is what lets us later delete a
// pool pod, keep its PVC, and re-create the pod under the same identity
// — the PVC is the durable handle.
func (b *PodBuilder) BuildPVC(namespace, imageRef string) (*corev1.PersistentVolumeClaim, error) {
	if b.AgentImage == "" {
		return nil, fmt.Errorf("pool: PodBuilder.AgentImage is required")
	}
	if imageRef == "" {
		return nil, fmt.Errorf("pool: imageRef is required")
	}
	hash := ImageHash(imageRef)
	storage := b.PVCStorage
	if storage.IsZero() {
		storage = resource.MustParse("10Gi")
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("pool-%s-", hash),
			Namespace:    namespace,
			Labels: map[string]string{
				LabelManagedBy:     LabelManagedByValue,
				LabelPoolImageHash: hash,
			},
			Annotations: map[string]string{
				AnnotationPoolImageRef: imageRef,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
			},
		},
	}
	if b.PVCStorageClass != "" {
		pvc.Spec.StorageClassName = ptr.To(b.PVCStorageClass)
	}
	return pvc, nil
}

// BuildPod returns the pool Pod for the given `name` (the PVC's assigned
// name from BuildPVC) and the OCI image reference. The Pod claims the
// PVC of the same name.
func (b *PodBuilder) BuildPod(namespace, name, imageRef string) (*corev1.Pod, error) {
	if b.AgentImage == "" {
		return nil, fmt.Errorf("pool: PodBuilder.AgentImage is required")
	}
	if imageRef == "" {
		return nil, fmt.Errorf("pool: imageRef is required")
	}
	agentPort := b.AgentPort
	if agentPort == 0 {
		agentPort = 7443
	}
	proxyPort := b.ProxyPort
	if proxyPort == 0 {
		proxyPort = 8080
	}
	capacity := b.Capacity
	if capacity <= 0 {
		capacity = DefaultCapacity
	}

	labels := map[string]string{
		LabelManagedBy:     LabelManagedByValue,
		LabelPoolImageHash: ImageHash(imageRef),
	}
	annotations := map[string]string{
		AnnotationPoolImageRef: imageRef,
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			// hostUsers=false enables user namespaces, mitigating the risk
			// of the privileged capabilities we still need below.
			//
			// Currently disabled (true) because kind nodes can't mount
			// sysfs inside a nested userns out of the box (containerd
			// needs subuid/subgid map config). Flip to false once the
			// target cluster supports it. See plan.md.
			HostUsers: ptr.To(true),
			Containers: []corev1.Container{{
				Name:  "pod-agent",
				Image: b.AgentImage,
				Args: []string{
					fmt.Sprintf("--grpc-port=%d", agentPort),
					fmt.Sprintf("--proxy-port=%d", proxyPort),
					"--ssh-port=2222",
					fmt.Sprintf("--capacity=%d", capacity),
					"--state-dir=/var/lib/sandboxes",
					"--image-dir=/sandbox-image",
				},
				Ports: []corev1.ContainerPort{
					{Name: "grpc", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP},
					{Name: "proxy", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
					{Name: "ssh", ContainerPort: 2222, Protocol: corev1.ProtocolTCP},
				},
				SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"SYS_ADMIN", "NET_ADMIN"},
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "sandbox-image", MountPath: "/sandbox-image", ReadOnly: true},
					{Name: "sandbox-state", MountPath: "/var/lib/sandboxes"},
					{Name: "sandbox-run", MountPath: "/run/sandboxes"},
				},
				// grpc_health_probe consults the pod-agent's
				// grpc.health.v1.Health endpoint; pod-agent only flips to
				// SERVING after its pre-flight checks (bwrap installed,
				// image volume mounted, state PVC writable, worker binary
				// present) pass. A TCP probe would go ready too early.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						GRPC: &corev1.GRPCAction{
							Port: agentPort,
						},
					},
					PeriodSeconds:    5,
					TimeoutSeconds:   3,
					FailureThreshold: 3,
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "sandbox-image",
					VolumeSource: corev1.VolumeSource{
						Image: &corev1.ImageVolumeSource{
							Reference:  imageRef,
							PullPolicy: corev1.PullIfNotPresent,
						},
					},
				},
				{
					Name: "sandbox-state",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: name,
						},
					},
				},
				{
					Name: "sandbox-run",
					// tmpfs-backed: holds overlay merged mountpoints and worker
					// unix sockets. Must be ephemeral and fast; never persisted.
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: ptr.To(resource.MustParse("1Gi")),
						},
					},
				},
			},
		},
	}
	return pod, nil
}
