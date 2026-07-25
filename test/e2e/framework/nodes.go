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

package framework

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// WorkerNode holds summary information about a schedulable worker node.
type WorkerNode struct {
	Name            string
	InstanceType    string
	AllocatableCPUs int64
	AllocatableRAM  int64
	AllocatablePods int64
}

func isControlPlaneNode(node *corev1.Node) bool {
	for k := range node.Labels {
		if strings.Contains(k, "control-plane") || strings.Contains(k, "master") {
			return true
		}
	}
	return false
}

func hasBlockingTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}
