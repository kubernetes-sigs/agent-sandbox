// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package framework

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// NodeLogOptions holds options for retrieving node logs.
type NodeLogOptions struct {
	Since        time.Time
	FilterByPod  types.NamespacedName
	ArtifactsDir string
}

// MustGetKubeletLogs retrieves kubelet journal logs from a kind node.
// It filters logs to show entries since the specified time.
func (cl *ClusterClient) MustGetKubeletLogs(nodeName string, opt NodeLogOptions) {
	cl.Helper()
	ctx := cl.Context()

	// Format time for journalctl --since flag
	sinceStr := opt.Since.UTC().Format("2006-01-02 15:04:05.000")
	stdout, stderr, err := cl.ExecuteOnNode(ctx, nodeName, []string{
		"journalctl", "-u", "kubelet", "--utc", "--since", sinceStr, "--no-pager",
	})
	if err != nil {
		cl.Fatalf("failed to get kubelet logs: %v (stderr: %s)", err, stderr)
	}

	p := filepath.Join(opt.ArtifactsDir, fmt.Sprintf("kubelet-%s.log", nodeName))
	if err := os.WriteFile(p, []byte(stdout), 0o644); err != nil {
		cl.Fatalf("failed to write kubelet logs to %s: %v", p, err)
	}
	cl.Logf("wrote kubelet logs to %s", p)
}

// MustGetContainerdLogs retrieves containerd journal logs from a kind node.
// It filters logs to show entries since the specified time.
func (cl *ClusterClient) MustGetContainerdLogs(nodeName string, opt NodeLogOptions) {
	cl.Helper()
	ctx := cl.Context()

	// Format time for journalctl --since flag
	sinceStr := opt.Since.UTC().Format("2006-01-02 15:04:05.000")
	stdout, stderr, err := cl.ExecuteOnNode(ctx, nodeName, []string{
		"journalctl", "-u", "containerd", "--utc", "--since", sinceStr, "--no-pager",
	})
	if err != nil {
		cl.Fatalf("failed to get containerd logs: %v (stderr: %s)", err, stderr)
	}

	p := filepath.Join(opt.ArtifactsDir, fmt.Sprintf("containerd-%s.log", nodeName))
	if err := os.WriteFile(p, []byte(stdout), 0o644); err != nil {
		cl.Fatalf("failed to write containerd logs to %s: %v", p, err)
	}
	cl.Logf("wrote containerd logs to %s", p)
}

// gatherNodeLogs retrieves kubelet and containerd logs from the kind node
// to understand timing between pod scheduling and container startup.
func (cl *ClusterClient) MustGetNodeLogs(nodeName string, opt NodeLogOptions) {
	cl.Helper()

	if nodeName == "" {
		cl.Fatalf("nodeName is required to get node logs")
	}

	cl.Logf("Gathering logs from node %s", nodeName)

	cl.MustGetKubeletLogs(nodeName, opt)

	cl.MustGetContainerdLogs(nodeName, opt)
}
