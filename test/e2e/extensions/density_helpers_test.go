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

package extensions

import (
	"flag"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

var (
	runPerfLoadTest  = flag.Bool("run-perf-load-test", false, "Whether to run the performance density load test.")
	nodeName         = flag.String("node-name", "", "The Kubernetes node to schedule sandboxes on. If empty, the first worker node is selected.")
	density          = flag.Int("density", 20, "The number of pods/sandboxes to provision.")
	imageTag         = flag.String("image-tag", "latest", "The tag of the sandbox image.")
	imagePrefix      = flag.String("image-prefix", "kind.local/", "The prefix of the sandbox image.")
	runtimeClassName = flag.String("runtime-class-name", "", "The RuntimeClassName to use for the sandbox pods.")
)

// AtomicTimeDuration is a wrapper around time.Duration that allows for concurrent updates and retrievals.
type AtomicTimeDuration struct {
	mu  sync.RWMutex
	d   time.Duration
	set bool
}

func (s *AtomicTimeDuration) Seconds() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Seconds()
}

func (s *AtomicTimeDuration) IsEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.set
}

func (s *AtomicTimeDuration) Set(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		s.d = d
		s.set = true
	}
}

func (s *AtomicTimeDuration) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.String()
}

func hashString(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

func getFirstWorkerNode(tc *framework.TestContext) (string, error) {
	ctx := tc.Context()
	nodes := &corev1.NodeList{}
	if err := tc.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found in the cluster")
	}

	// Prefer worker nodes without control-plane or master roles for Kind clusters,
	// and without NoSchedule taints
	for _, node := range nodes.Items {
		isControlPlane := false
		for k := range node.Labels {
			if strings.Contains(k, "control-plane") || strings.Contains(k, "master") {
				isControlPlane = true
				break
			}
		}
		hasNoScheduleTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				hasNoScheduleTaint = true
				break
			}
		}
		if !isControlPlane && !hasNoScheduleTaint {
			return node.Name, nil
		}
	}

	// Fallback to the first node
	return nodes.Items[0].Name, nil
}
