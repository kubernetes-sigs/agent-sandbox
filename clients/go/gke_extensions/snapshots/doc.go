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

// Package snapshots is a Go client extension for interacting with the GKE Pod
// Snapshot feature from within the agent-sandbox framework. It allows Go users
// to trigger snapshots of a running sandbox and restore a new sandbox from a
// recently created snapshot — mirroring the functionality of the Python client
// at clients/python/agentic-sandbox-client/k8s_agent_sandbox/gke_extensions/snapshots.
//
// # Main types
//
//   - [PodSnapshotClient] — the top-level entry point. Wraps [sandbox.Client]
//     and validates at construction time that the GKE Pod Snapshot CRDs are
//     installed. All sandboxes it creates are [SandboxWithSnapshotSupport].
//
//   - [SandboxWithSnapshotSupport] — wraps a sandbox handle with snapshot
//     lifecycle operations: Suspend, Resume, IsRestoredFromSnapshot, IsActive.
//
//   - [SnapshotEngine] — low-level engine for Create / List / Delete / DeleteAll
//     operations against PodSnapshot and PodSnapshotManualTrigger CRDs.
//
// # Cluster prerequisites
//
// GKE Pod Snapshots require:
//
//  1. A GKE Standard cluster running gVisor (version ≥ 1.35.2-gke.1842000).
//
//  2. The PodSnapshotStorageConfig and PodSnapshotPolicy CRDs installed and
//     configured with a GCS bucket and appropriate IAM permissions.
//
//  3. A PodSnapshotPolicy whose snapshotGroupingRules include the
//     "agents.x-k8s.io/sandbox-name-hash" label. Example:
//
//     apiVersion: podsnapshot.gke.io/v1
//     kind: PodSnapshotPolicy
//     metadata:
//     name: my-policy
//     namespace: default
//     spec:
//     storageConfigName: my-storage-config
//     selector:
//     matchLabels:
//     app: agent-sandbox-workload
//     triggerConfig:
//     type: manual
//     postCheckpoint: resume
//     snapshotGroupingRules:
//     groupByLabelValue:
//     labels: ["agents.x-k8s.io/sandbox-name-hash"]
//     groupRetentionPolicy:
//     maxSnapshotCountPerGroup: 3
//
// # Usage
//
//	client, err := snapshots.NewPodSnapshotClient(ctx, sandbox.Options{
//	    RestConfig: restConfig,
//	    Logger:     logger,
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.DeleteAll(ctx)
//
//	sb, err := client.CreateSandbox(ctx, "my-template", "default")
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Take a manual snapshot.
//	resp := sb.Snapshots().Create(ctx, "my-snapshot", 3*time.Minute)
//	if resp.Success {
//	    fmt.Println("snapshot UID:", resp.SnapshotUID)
//	}
//
//	// Suspend (optionally snapshots first) and later resume.
//	suspendResp := sb.Suspend(ctx, true, 3*time.Minute)
//	resumeResp  := sb.Resume(ctx, 3*time.Minute)
//	fmt.Println("restored from snapshot:", resumeResp.RestoredFromSnapshot)
package snapshots
