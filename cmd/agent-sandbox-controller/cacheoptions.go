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

package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/agent-sandbox/controllers"
)

// buildCacheOptions constructs the manager's informer cache configuration.
//
// Always applied:
//   - Strip metadata.managedFields from every cached object (CRs included).
//     Nothing in this repo reads managedFields, other writers inflate it (the
//     kubelet server-side-applies pod status on every update), and every
//     write here is either a merge patch diffed between two equally-stripped
//     copies (managedFields can never appear in the diff) or an
//     update/create, where an absent managedFields means "leave server-side
//     field management unchanged". Pure decode-CPU/memory win.
//   - The Pod cache additionally drops the pod spec except spec.nodeName —
//     the only spec field any controller reads (see PodCacheTransform).
//
// With scopeToTrackingLabel, the Pod and Service informers are additionally
// restricted to objects carrying the sandbox tracking label; see the
// --cache-label-selectors flag help for the trade-off.
//
// A single *corev1.Pod key is reused for every ByObject access: ByObject is
// keyed by pointer identity for lookups within this function, so writing the
// scoped entry through a second &corev1.Pod{} literal would ADD a duplicate
// Pod entry instead of replacing the unscoped one.
func buildCacheOptions(scopeToTrackingLabel bool) (cache.Options, error) {
	pod := &corev1.Pod{}
	opts := cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			pod: {Transform: controllers.PodCacheTransform},
		},
	}
	if scopeToTrackingLabel {
		trackedOnly, err := labels.NewRequirement(controllers.SandboxNameHashLabel, selection.Exists, nil)
		if err != nil {
			return cache.Options{}, fmt.Errorf("building cache label selector: %w", err)
		}
		sel := labels.NewSelector().Add(*trackedOnly)
		podEntry := opts.ByObject[pod]
		podEntry.Label = sel
		opts.ByObject[pod] = podEntry
		opts.ByObject[&corev1.Service{}] = cache.ByObject{Label: sel}
	}
	return opts, nil
}
