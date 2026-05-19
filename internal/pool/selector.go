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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// Selector chooses an existing pool pod that can host a new sandbox tenant.
// It returns ErrNoCapacity if no eligible pool pod has free slots; callers
// react by creating a new pool pod.
type Selector struct {
	Client   client.Client
	Capacity int // tenants per pool pod; 0 → DefaultCapacity
}

// ErrNoCapacity indicates that no eligible pool pod has a free slot.
var ErrNoCapacity = fmt.Errorf("pool: no pool pod with free capacity")

// ErrWaitingForPoolPod indicates that pool pods exist for this image but
// none is ready yet. Caller should wait (requeue) rather than provision
// a new pool pod — the not-yet-ready one is likely about to come online.
var ErrWaitingForPoolPod = fmt.Errorf("pool: pool pod exists but not ready")

// Choose returns the name of the pool pod that should host this sandbox.
// If sandbox.Status.Host.PodName is already set, it is returned unchanged
// (sticky binding survives controller restarts).
//
// The selector matches pool pods by:
//   - LabelManagedBy == LabelManagedByValue
//   - LabelPoolImageHash == ImageHash(sandbox.Spec.Image.Reference)
//   - optional spec.pool.matchLabels / spec.pool.name overrides
//   - pod.Status.Phase == Running and pod is Ready
//
// Tenants assigned to a pod are counted by listing Sandbox CRs whose
// status.host.podName == pod.Name.
func (s *Selector) Choose(ctx context.Context, sandbox *sandboxv1alpha1.ManagedSandbox) (string, error) {
	if sandbox.Status.Host != nil && sandbox.Status.Host.PodName != "" {
		return sandbox.Status.Host.PodName, nil
	}

	matchLabels := map[string]string{
		LabelManagedBy:     LabelManagedByValue,
		LabelPoolImageHash: ImageHash(sandbox.Spec.Image.Reference),
	}

	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods,
		client.InNamespace(sandbox.Namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(matchLabels)},
	); err != nil {
		return "", fmt.Errorf("pool: list pods: %w", err)
	}

	capacity := s.Capacity
	if capacity <= 0 {
		capacity = DefaultCapacity
	}

	// Stable iteration: sort by name so two replicas of the controller pick
	// the same pod when racing on Choose.
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })

	hasPending := false
	for i := range pods.Items {
		p := &pods.Items[i]
		if !IsPodReady(p) {
			// Not-ready pool pods count as "warming up": concurrent
			// reconciles for the same image should wait for them
			// rather than each provisioning their own new pool pod.
			// Treating them as opaque-blockers is the simplest dedup
			// today; once we mark unhealthy pods explicitly we can
			// skip those here.
			hasPending = true
			continue
		}
		count, err := s.countTenants(ctx, sandbox.Namespace, p.Name)
		if err != nil {
			return "", err
		}
		if count < capacity {
			return p.Name, nil
		}
	}

	if hasPending {
		return "", ErrWaitingForPoolPod
	}
	return "", ErrNoCapacity
}

func (s *Selector) countTenants(ctx context.Context, namespace, podName string) (int, error) {
	list := &sandboxv1alpha1.ManagedSandboxList{}
	// Cheap O(N) scan; if it becomes a bottleneck, add a field index on
	// status.host.podName via builder.IndexField in SetupWithManager.
	if err := s.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("pool: list sandboxes: %w", err)
	}
	n := 0
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Status.Host != nil && sb.Status.Host.PodName == podName {
			n++
		}
	}
	return n, nil
}

func IsPodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
