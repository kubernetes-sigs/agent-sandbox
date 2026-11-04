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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ResizeOption configures per-call behavior of ResizePodResources.
type ResizeOption func(*resizeOptions)

type resizeOptions struct {
	syncTemplate bool
}

// WithoutTemplateSync disables the default Sandbox spec synchronization.
// By default ResizePodResources also updates Sandbox.Spec.PodTemplate so the
// Sandbox spec matches the resized Pod; without that sync the two drift
// apart, which can surprise later template-driven operations.
func WithoutTemplateSync() ResizeOption {
	return func(o *resizeOptions) {
		o.syncTemplate = false
	}
}

// ResizePodResources requests an in-place resize of one container's CPU and
// memory resources on the Sandbox's backing Pod via the Pod /resize
// subresource (KEP-1287). The Pod is not restarted and keeps its identity.
//
// This operates on the Pod directly instead of going through the Sandbox
// API, per the project guidance (issue #612): the Sandbox spec stays a
// user-owned declaration, and the resize does not freeze a KEP-1287 subset
// into the Sandbox API surface.
//
// By default the Sandbox spec is synchronized afterwards so
// Sandbox.Spec.PodTemplate reflects the new sizing; pass WithoutTemplateSync
// to skip that write.
//
// Resizing is opt-in on the cluster side: every container on the Pod must
// have a RestartPolicy of RestartNotRequired for CPU and memory, and the
// cluster must serve the Pod resize subresource. The call returns the API
// server's error unchanged (wrapped) when a resize is rejected.
//
// Required RBAC for the caller's credentials: patch on pods/resize, and
// (unless WithoutTemplateSync is used) patch on sandboxes.
//
// Example:
//
//	err := sb.ResizePodResources(ctx, "workspace", corev1.ResourceRequirements{
//	    Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
//	    Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
//	})
func (s *Sandbox) ResizePodResources(ctx context.Context, containerName string, resources corev1.ResourceRequirements, opts ...ResizeOption) error {
	cfg := resizeOptions{syncTemplate: true}
	for _, opt := range opts {
		opt(&cfg)
	}
	if s.k8s == nil || s.k8s.CoreClient == nil {
		return fmt.Errorf("sandbox: ResizePodResources requires a K8sHelper with a CoreClient; open the Sandbox with Kubernetes options")
	}

	podName := s.podName
	if podName == "" {
		// Mirrors the controller's pod naming: the backing Pod takes the
		// Sandbox's name (see extractState).
		podName = s.sandboxName
	}
	namespace := s.opts.Namespace

	patch, err := containerResourcesPatch(podName, containerName, resources)
	if err != nil {
		return err
	}

	// Submit the resize against the /resize subresource only; the Pod is
	// never updated through the main resource.
	result := s.k8s.CoreClient.RESTClient().
		Patch(types.StrategicMergePatchType).
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("resize").
		Body(patch).
		Do(ctx)
	if err := result.Error(); err != nil {
		return fmt.Errorf("sandbox: resize pod %q container %q: %w", podName, containerName, err)
	}

	if !cfg.syncTemplate {
		return nil
	}
	return s.syncTemplateResources(ctx, containerName, resources)
}

// syncTemplateResources updates Sandbox.Spec.PodTemplate so the declared
// template matches the resized Pod. The write is skipped when the template
// already carries the requested resources.
func (s *Sandbox) syncTemplateResources(ctx context.Context, containerName string, resources corev1.ResourceRequirements) error {
	if s.k8s.AgentsClient == nil {
		return fmt.Errorf("sandbox: template sync requires a K8sHelper with an AgentsClient")
	}
	sb, err := s.k8s.AgentsClient.Sandboxes(s.opts.Namespace).Get(ctx, s.sandboxName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: get sandbox %q for template sync: %w", s.sandboxName, err)
	}
	for i := range sb.Spec.PodTemplate.Spec.Containers {
		if sb.Spec.PodTemplate.Spec.Containers[i].Name != containerName {
			continue
		}
		if equality.Semantic.DeepEqual(sb.Spec.PodTemplate.Spec.Containers[i].Resources, resources) {
			return nil
		}
		patch, err := sandboxTemplateResourcesPatch(sb.Name, containerName, resources)
		if err != nil {
			return err
		}
		if _, err := s.k8s.AgentsClient.RESTClient().
			Patch(types.MergePatchType).
			Namespace(s.opts.Namespace).
			Resource("sandboxes").
			Name(sb.Name).
			Body(patch).
			Do(ctx).
			Get(); err != nil {
			return fmt.Errorf("sandbox: sync sandbox %q template resources: %w", sb.Name, err)
		}
		return nil
	}
	return fmt.Errorf("sandbox: container %q not found in sandbox %q pod template", containerName, s.sandboxName)
}

// containerResourcesPatch builds a strategic-merge style patch that sets the
// resources of the named container, matching containers by name.
func containerResourcesPatch(podName, containerName string, resources corev1.ResourceRequirements) ([]byte, error) {
	patch := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				containerResourcesPatchEntry(containerName, resources),
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build resize patch for pod %q: %w", podName, err)
	}
	return body, nil
}

// sandboxTemplateResourcesPatch builds the merge patch that mirrors resized
// resources into the Sandbox's pod template.
func sandboxTemplateResourcesPatch(sandboxName, containerName string, resources corev1.ResourceRequirements) ([]byte, error) {
	patch := map[string]any{
		"spec": map[string]any{
			"podTemplate": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						containerResourcesPatchEntry(containerName, resources),
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build template patch for sandbox %q: %w", sandboxName, err)
	}
	return body, nil
}

func containerResourcesPatchEntry(containerName string, resources corev1.ResourceRequirements) map[string]any {
	return map[string]any{
		"name":      containerName,
		"resources": resources,
	}
}
