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
	"k8s.io/apimachinery/pkg/types"
)

// ResizePodResources requests an in-place resize of one container's CPU and
// memory resources on the Sandbox's backing Pod via the Pod /resize
// subresource (KEP-1287). The Pod keeps its identity; whether the resized
// container restarts is controlled by its per-container resizePolicy
// (NotRequired avoids a restart, RestartContainer allows one).
//
// This operates on the Pod directly instead of going through the Sandbox
// API, per the project guidance (issue #612): the Sandbox spec stays a
// user-owned declaration, and the resize does not freeze a KEP-1287 subset
// into the Sandbox API surface. The Sandbox spec is deliberately left
// untouched: it cannot track live changes from other /resize callers such as
// VPA, and callers who want the new size reflected in
// Sandbox.Spec.PodTemplate can patch the Sandbox themselves.
//
// The cluster must serve the Pod resize subresource. The call returns the
// API server's error unchanged (wrapped) when a resize is rejected.
//
// Required RBAC for the caller's credentials: patch on pods/resize.
//
// Example:
//
//	err := sb.ResizePodResources(ctx, "workspace", corev1.ResourceRequirements{
//	    Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
//	    Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
//	})
func (s *Sandbox) ResizePodResources(ctx context.Context, containerName string, resources corev1.ResourceRequirements) error {
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
	return nil
}

// containerResourcesPatch builds a strategic-merge style patch that sets the
// resources of the named container, matching containers by name.
func containerResourcesPatch(podName, containerName string, resources corev1.ResourceRequirements) ([]byte, error) {
	patch := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":      containerName,
					"resources": resources,
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build resize patch for pod %q: %w", podName, err)
	}
	return body, nil
}
