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
	"bufio"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// rbacDenialMarker is the fixed prefix apimachinery renders for RBAC
// rejections (`<resource> "<name>" is forbidden: User "..." cannot ...`).
// The trailing ": User" keeps benign controller messages that merely
// contain the word "forbidden" from matching.
const rbacDenialMarker = "is forbidden: User"

// ScanControllerRBACDenials fetches the logs of every controller pod and
// returns any lines showing an RBAC denial. The e2e suites call this from
// TestMain after all tests have run: the suite exercises every controller
// code path, so a missing RBAC verb that only degrades silently (event
// emission, best-effort patches) still surfaces here.
func ScanControllerRBACDenials(ctx context.Context) ([]string, error) {
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: GetKubeconfig()},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}

	pods, err := clientset.CoreV1().Pods("agent-sandbox-system").List(
		ctx, metav1.ListOptions{LabelSelector: "app=agent-sandbox-controller"},
	)
	if err != nil {
		return nil, fmt.Errorf("listing controller pods: %w", err)
	}

	var denials []string
	for _, pod := range pods.Items {
		stream, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("streaming logs for pod %s: %w", pod.Name, err)
		}
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if line := scanner.Text(); strings.Contains(line, rbacDenialMarker) {
				denials = append(denials, fmt.Sprintf("%s: %s", pod.Name, line))
			}
		}
		err = scanner.Err()
		stream.Close()
		if err != nil {
			return nil, fmt.Errorf("reading logs for pod %s: %w", pod.Name, err)
		}
	}
	return denials, nil
}
