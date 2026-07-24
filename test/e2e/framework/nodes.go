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
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// WorkerNode holds summary information about a schedulable worker node.
type WorkerNode struct {
	Name            string
	InstanceType    string
	AllocatableCPUs int64
	AllocatablePods int64
}

// WorkerNodes returns all schedulable worker nodes in the cluster, excluding
// control-plane/master nodes, unschedulable nodes, and nodes with NoSchedule
// or NoExecute taints.
func (cl *ClusterClient) WorkerNodes(ctx context.Context) ([]WorkerNode, error) {
	cl.Helper()

	var nodeList corev1.NodeList
	if err := cl.List(ctx, &nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	var workers []WorkerNode
	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		if isControlPlaneNode(node) || node.Spec.Unschedulable || hasBlockingTaint(node) {
			continue
		}

		w := WorkerNode{Name: node.Name}
		w.InstanceType = node.Labels["node.kubernetes.io/instance-type"]
		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
			w.AllocatableCPUs = cpu.Value()
		}
		if pods := node.Status.Allocatable.Pods(); pods != nil {
			w.AllocatablePods = pods.Value()
		}
		workers = append(workers, w)
	}
	return workers, nil
}

// ClusterCPUCapacity returns the total allocatable CPU cores across all
// schedulable worker nodes.
func (cl *ClusterClient) ClusterCPUCapacity(ctx context.Context) (int64, error) {
	cl.Helper()

	workers, err := cl.WorkerNodes(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, w := range workers {
		total += w.AllocatableCPUs
	}
	return total, nil
}

// ClusterPodCapacity returns the total allocatable pod slots across all
// schedulable worker nodes.
func (cl *ClusterClient) ClusterPodCapacity(ctx context.Context) (int64, error) {
	cl.Helper()

	workers, err := cl.WorkerNodes(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, w := range workers {
		total += w.AllocatablePods
	}
	return total, nil
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
