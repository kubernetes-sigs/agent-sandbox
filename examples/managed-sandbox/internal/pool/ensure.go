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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provisioner creates pool pods (and their backing PVCs) on demand when
// Selector.Choose returns ErrNoCapacity.
//
// Pool pods are bare Pods, not owned by any custom resource — the
// controller is responsible for GC. They are labeled with LabelManagedBy
// so the controller can list and reconcile them independently of any
// Sandbox CR.
type Provisioner struct {
	Client  client.Client
	Builder PodBuilder
	// FieldOwner stamps server-side-apply ownership on PVC and Pod
	// creates. Defaults to the same value the main Sandbox controller
	// uses; callers pass it through to keep ownership coherent.
	FieldOwner client.FieldOwner
	// CreateService, when true, materializes a per-pool-pod headless
	// Service alongside the Pod and PVC. The Service is the addressable
	// backend for HTTPRoutes; without HTTPRoute mode (no Gateway parent
	// configured) the Service has no consumer, so we skip it by default.
	CreateService bool
}

// CreateNew creates a fresh pool pod (and its backing PVC) for the given
// OCI image and returns the new pod's name. It is the caller's job to
// have already established that more capacity is needed — this function
// always creates new resources.
//
// Naming: the PVC is created with `GenerateName: "pool-<hash>-"`; the
// API server picks a unique suffix that we reuse as the Pod's name. PVC
// and Pod therefore share an identical name (no `-state` suffix), which
// lets us later delete the Pod and re-create it under the same identity
// backed by the same PVC.
//
// No per-pod Service is created. Callers reach the pod-agent and SSH
// listener via Pod IP today; if a Gateway-API HTTPRoute backend is
// eventually wired up we'll need to materialize a Service (or use an
// equivalent EndpointSlice-style backend).
func (p *Provisioner) CreateNew(ctx context.Context, namespace, imageRef string) (string, error) {
	if imageRef == "" {
		return "", fmt.Errorf("pool: imageRef is required")
	}
	pvc, err := p.Builder.BuildPVC(namespace, imageRef)
	if err != nil {
		return "", err
	}
	if err := p.Client.Create(ctx, pvc, p.createOpts()...); err != nil {
		return "", fmt.Errorf("pool: create pvc: %w", err)
	}
	name := pvc.Name // populated by the API server from GenerateName.

	pod, err := p.Builder.BuildPod(namespace, name, imageRef)
	if err != nil {
		return "", err
	}
	if p.CreateService {
		// Service selector targets a name-scoped label on the pod.
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[LabelPoolPod] = name
	}
	if err := p.Client.Create(ctx, pod, p.createOpts()...); err != nil && !k8serrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("pool: create pod %s: %w", name, err)
	}

	if p.CreateService {
		svc := buildPoolPodService(namespace, name, &p.Builder)
		if err := p.Client.Create(ctx, svc, p.createOpts()...); err != nil && !k8serrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("pool: create service %s: %w", name, err)
		}
	}
	return name, nil
}

// RecreatePod recreates a pool Pod that was deleted, reusing its
// existing PVC (which holds the per-tenant overlay state). The PVC's
// `AnnotationPoolImageRef` annotation supplies the OCI image reference;
// the pod takes the PVC's name (per the GenerateName scheme used in
// CreateNew). Returns a NotFound-shaped error if the PVC itself is gone
// — callers fall back to a fresh CreateNew binding.
//
// Service is intentionally not re-ensured here: when CreateService=true,
// the Service was created without an owner ref and survives a pod
// deletion. If a future operator deletes both pod and Service we'll
// need a separate ensure step.
func (p *Provisioner) RecreatePod(ctx context.Context, namespace, name string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pvc); err != nil {
		return fmt.Errorf("pool: get pvc %s: %w", name, err)
	}
	imageRef := pvc.Annotations[AnnotationPoolImageRef]
	if imageRef == "" {
		return fmt.Errorf("pool: PVC %s missing %s annotation; cannot recreate pod",
			name, AnnotationPoolImageRef)
	}
	pod, err := p.Builder.BuildPod(namespace, name, imageRef)
	if err != nil {
		return err
	}
	if p.CreateService {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[LabelPoolPod] = name
	}
	if err := p.Client.Create(ctx, pod, p.createOpts()...); err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("pool: recreate pod %s: %w", name, err)
	}
	return nil
}

func (p *Provisioner) createOpts() []client.CreateOption {
	if p.FieldOwner == "" {
		return nil
	}
	return []client.CreateOption{p.FieldOwner}
}

// buildPoolPodService returns a headless (ClusterIP=None) Service that
// targets a single pool pod via the LabelPoolPod selector. Headless is
// fine because only ever one pod backs it; clients resolve the pod IP
// directly through DNS.
func buildPoolPodService(namespace, podName string, builder *PodBuilder) *corev1.Service {
	proxyPort := builder.ProxyPort
	if proxyPort == 0 {
		proxyPort = 8080
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
				LabelPoolPod:   podName,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{LabelPoolPod: podName},
			Ports: []corev1.ServicePort{
				{
					Name:       "proxy",
					Port:       proxyPort,
					TargetPort: intstrFromInt32(proxyPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "ssh",
					Port:       2222,
					TargetPort: intstrFromInt32(2222),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// GetPod fetches a pool pod by name, returning (nil, nil) on NotFound.
func GetPod(ctx context.Context, c client.Client, namespace, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pod, nil
}
