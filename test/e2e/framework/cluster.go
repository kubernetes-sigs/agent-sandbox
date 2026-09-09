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
	"encoding/json"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
)

// ClusterInfo holds pre-computed cluster topology and capacity gathered from
// a single node-list call plus lightweight API queries.
type ClusterInfo struct {
	Workers            []WorkerNode
	Identity           string
	KubernetesVersion  string
	SandboxVersion     string
	Provider           string
	TotalCPUCapacity   int64
	TotalRAMCapacity   int64
	TotalPodCapacity   int64
	PreexistingPods    int
	AllocatedCPUMillis int64
	AllocatedRAM       int64
}

// ClusterInfo lists nodes once and returns aggregated cluster topology:
// filtered worker nodes, total CPU/RAM/pod capacity, cluster identity,
// Kubernetes version, cloud provider, and current resource usage.
func (cl *ClusterClient) ClusterInfo(ctx context.Context) (*ClusterInfo, error) {
	cl.Helper()

	var nodeList corev1.NodeList
	if err := cl.List(ctx, &nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	info := &ClusterInfo{}
	controlPlaneNames := make(map[string]struct{})

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if isControlPlaneNode(node) {
			controlPlaneNames[node.Name] = struct{}{}
			continue
		}
		if node.Spec.Unschedulable || hasBlockingTaint(node) {
			continue
		}

		w := WorkerNode{Name: node.Name}
		w.InstanceType = node.Labels["node.kubernetes.io/instance-type"]
		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
			w.AllocatableCPUs = cpu.MilliValue() / 1000
		}
		if mem := node.Status.Allocatable.Memory(); mem != nil {
			w.AllocatableRAM = mem.Value()
		}
		if pods := node.Status.Allocatable.Pods(); pods != nil {
			w.AllocatablePods = pods.Value()
		}

		if info.Provider == "" && node.Spec.ProviderID != "" {
			info.Provider = detectProvider(node.Spec.ProviderID)
		}

		info.TotalCPUCapacity += w.AllocatableCPUs
		info.TotalRAMCapacity += w.AllocatableRAM
		info.TotalPodCapacity += w.AllocatablePods
		info.Workers = append(info.Workers, w)
	}

	if info.Provider == "" {
		info.Provider = "baremetal"
	}

	info.Identity = clusterIdentity(info.Workers)
	info.KubernetesVersion = cl.serverVersion(ctx)
	info.SandboxVersion = cl.sandboxVersion(ctx)

	if err := cl.gatherPodUsage(ctx, controlPlaneNames, info); err != nil {
		return nil, err
	}

	return info, nil
}

func (cl *ClusterClient) serverVersion(ctx context.Context) string {
	if cl.restConfig == nil {
		return "unknown"
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cl.restConfig)
	if err != nil {
		return "unknown"
	}
	body, err := dc.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "unknown"
	}
	var v version.Info
	if err := json.Unmarshal(body, &v); err != nil {
		return "unknown"
	}
	return v.GitVersion
}

func (cl *ClusterClient) sandboxVersion(ctx context.Context) string {
	if v := os.Getenv("SANDBOX_VERSION"); v != "" {
		return v
	}
	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}
	if err := cl.Get(ctx, key, &deploy); err != nil {
		return "unknown"
	}
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if img := c.Image; img != "" {
			if idx := strings.LastIndex(img, "@"); idx >= 0 {
				return img[idx+1:]
			}
			lastSlash := strings.LastIndex(img, "/")
			if idx := strings.LastIndex(img, ":"); idx > lastSlash {
				return img[idx+1:]
			}
			return img
		}
	}
	return "unknown"
}

func (cl *ClusterClient) gatherPodUsage(ctx context.Context, controlPlaneNames map[string]struct{}, info *ClusterInfo) error {
	var podList corev1.PodList
	if err := cl.List(ctx, &podList); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if _, onCP := controlPlaneNames[pod.Spec.NodeName]; onCP {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		info.PreexistingPods++
		for j := range pod.Spec.Containers {
			req := pod.Spec.Containers[j].Resources.Requests
			info.AllocatedCPUMillis += req.Cpu().MilliValue()
			info.AllocatedRAM += req.Memory().Value()
		}
	}
	return nil
}

func detectProvider(providerID string) string {
	if scheme, _, ok := strings.Cut(providerID, "://"); ok {
		return scheme
	}
	return providerID
}

func clusterIdentity(workers []WorkerNode) string {
	if id := os.Getenv("SANDBOX_CLUSTER_ID"); id != "" {
		return id
	}
	if len(workers) == 0 {
		return "unknown"
	}
	if len(workers) == 1 {
		return workers[0].Name
	}
	names := make([]string, len(workers))
	for i, w := range workers {
		names[i] = w.Name
	}
	prefix := longestCommonPrefix(names)
	prefix = strings.TrimRight(prefix, "-_.")
	if prefix == "" {
		return workers[0].Name
	}
	return prefix
}

func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		for i := range prefix {
			if i >= len(s) || prefix[i] != s[i] {
				prefix = prefix[:i]
				break
			}
		}
	}
	return prefix
}
