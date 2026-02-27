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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

const (
	// pvcSandboxName is the sandbox name used throughout the test.
	pvcSandboxName = "pvc-persistence-sandbox"

	// workspacesPVCTemplateName is the volumeClaimTemplate name; the controller
	// creates the PVC as "<templateName>-<sandboxName>".
	workspacesPVCTemplateName = "workspaces"

	// dataFilePath is the path inside the container where we write the generated file.
	dataFilePath = "/workspaces/data.txt"

	// dataContent is the data written to the PVC.
	dataContent = "# Agent sandbox test data"
)

// TestPVCPersistenceAcrossReplicas tests the replicas=0/1 lifecycle of a sandbox with a PVC:
//
//  1. Creates a sandbox (busybox container) with a PVC mounted at /workspaces.
//  2. Waits for sandbox to be ready, then writes a data file onto the PVC.
//  3. Sets replicas=0: verifies pod is deleted, PVC and Sandbox CR are preserved.
//  4. Sets replicas=1: verifies sandbox becomes ready again with the same PVC.
//  5. Verifies the data file still exists on the PVC in the new pod.
func TestPVCPersistenceAcrossReplicas(t *testing.T) {
	tc := framework.NewTestContext(t)
	ctx := t.Context()

	nameHash := NameHash(pvcSandboxName)

	// Namespace
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("pvc-persistence-test-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(ctx, ns))

	// Sandbox with PVC
	sandboxObj := pvcSandbox(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(ctx, sandboxObj))

	pvcName := fmt.Sprintf("%s-%s", workspacesPVCTemplateName, pvcSandboxName)
	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Name = pvcName
	pvc.Namespace = ns.Name

	// Wait for sandbox to be ready with 1 replica
	t.Logf("Waiting for sandbox to become ready (replicas=1)")
	require.NoError(t, tc.WaitForObject(ctx, sandboxObj,
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       pvcSandboxName,
			ServiceFQDN:   fmt.Sprintf("%s.%s.svc.cluster.local", pvcSandboxName, ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Message:            "Pod is Ready; Service Exists",
					ObservedGeneration: 1,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	))
	t.Logf("Sandbox is ready with 1 replica")

	// Verify PVC exists
	tc.MustExist(pvc)
	t.Logf("PVC %s/%s exists", ns.Name, pvcName)

	// Generating code on PVC
	t.Logf("Generating code on PVC at %s", dataFilePath)
	// We use a dynamic timestamp to ensure we're verifying the exact content generated in this run.
	generatedContent := fmt.Sprintf("# Dynamic code generated at %s\n\n%s", time.Now().Format(time.RFC3339), dataContent)
	require.NoError(t, execWriteFileToPodWithRetry(ctx, ns.Name, pvcSandboxName, dataFilePath, generatedContent))

	// Verify the write succeeded by reading back.
	content, err := execReadFileFromPodWithRetry(ctx, ns.Name, pvcSandboxName, dataFilePath)
	require.NoError(t, err, "failed to read back the file right after writing")
	require.Contains(t, content, "Agent sandbox test data", "file contents wrong right after write")
	t.Logf("Write verified: file is readable in original pod")

	// Scale down to replicas=0
	t.Logf("Setting replicas=0")
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.Replicas = ptr.To(int32(0))
	})

	// Wait for the sandbox status to reflect replicas=0
	t.Logf("Waiting for sandbox status to show replicas=0...")
	require.NoError(t, tc.WaitForObject(ctx, sandboxObj,
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       pvcSandboxName,
			ServiceFQDN:   fmt.Sprintf("%s.%s.svc.cluster.local", pvcSandboxName, ns.Name),
			Replicas:      0,
			LabelSelector: "",
			Conditions: []metav1.Condition{
				{
					Message:            "Pod does not exist, replicas is 0; Service Exists",
					ObservedGeneration: 2,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	))
	t.Logf("Sandbox status shows replicas=0")

	// Pod must be gone
	pod := &corev1.Pod{}
	pod.Name = pvcSandboxName
	pod.Namespace = ns.Name
	require.NoError(t, tc.WaitForObjectNotFound(ctx, pod))
	t.Logf("Pod deleted")

	// Sandbox CR still exists
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: pvcSandboxName, Namespace: ns.Name}, sandboxObj))
	t.Logf("Sandbox CR still exists")

	// PVC must still exist
	tc.MustExist(pvc)
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ns.Name}, pvc))
	originalPVCUID := pvc.UID
	t.Logf("PVC %s/%s still exists (UID=%s)", ns.Name, pvcName, originalPVCUID)

	// Scale back up to replicas=1
	t.Logf("Setting replicas=1")
	framework.MustUpdateObject(tc.ClusterClient, sandboxObj, func(obj *sandboxv1alpha1.Sandbox) {
		obj.Spec.Replicas = ptr.To(int32(1))
	})

	// Wait for the sandbox status to be ready with 1 replica again
	t.Logf("Waiting for sandbox to show replicas=1 and Ready=True...")
	require.NoError(t, tc.WaitForObject(ctx, sandboxObj,
		predicates.SandboxHasStatus(sandboxv1alpha1.SandboxStatus{
			Service:       pvcSandboxName,
			ServiceFQDN:   fmt.Sprintf("%s.%s.svc.cluster.local", pvcSandboxName, ns.Name),
			Replicas:      1,
			LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
			Conditions: []metav1.Condition{
				{
					Message:            "Pod is Ready; Service Exists",
					ObservedGeneration: 3,
					Reason:             "DependenciesReady",
					Status:             "True",
					Type:               "Ready",
				},
			},
		}),
	))
	t.Logf("Sandbox is ready again with 1 replica")

	// The same PVC was reused (not recreated)
	require.NoError(t, tc.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ns.Name}, pvc))
	require.Equal(t, originalPVCUID, pvc.UID,
		"PVC UID changed — a new PVC was created instead of reusing the original")
	t.Logf("Same PVC reused (UID=%s)", pvc.UID)

	// Verify generated code survived the replicas=0/1 cycle
	t.Logf("Verifying generated code persists in the new pod")
	content, err = execReadFileFromPodWithRetry(ctx, ns.Name, pvcSandboxName, dataFilePath)
	require.NoError(t, err, "failed to read generated code from pod after replicas=1 restore")
	require.Contains(t, content, "Agent sandbox test data",
		"expected dynamic content not found after PVC reattach — data was lost")
	t.Logf("SUCCESS: generated code persisted across replicas=0/1 cycle")
}

// pvcSandbox returns a Sandbox spec with a PVC mounted at /workspaces.
func pvcSandbox(ns string) *sandboxv1alpha1.Sandbox {
	return &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcSandboxName,
			Namespace: ns,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			Replicas: ptr.To(int32(1)),
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "workspace",
							Image: "busybox",
							// Python container keeps running if we give it a sleep command.
							Command: []string{"sh", "-c", "sleep infinity"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      workspacesPVCTemplateName,
									MountPath: "/workspaces",
								},
							},
						},
					},
				},
			},

			VolumeClaimTemplates: []sandboxv1alpha1.PersistentVolumeClaimTemplate{
				{
					EmbeddedObjectMetadata: sandboxv1alpha1.EmbeddedObjectMetadata{
						Name: workspacesPVCTemplateName,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}
}

// execWriteFileToPod writes content to a file inside a running pod via kubectl exec.
// The -i flag is required to forward stdin to the remote process.
func execWriteFileToPod(ctx context.Context, namespace, podName, filePath, content string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-i", "-n", namespace, podName, "--",
		"sh", "-c", fmt.Sprintf("cat > %s", filePath))
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl exec write to %s failed: %w (stderr: %s)", filePath, err, stderr.String())
	}
	return nil
}

// execWriteFileToPodWithRetry retries the write until the container is exec-able
// (useful right after a pod starts up).
func execWriteFileToPodWithRetry(ctx context.Context, namespace, podName, filePath, content string) error {
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		err := execWriteFileToPod(ctx, namespace, podName, filePath, content)
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("timed out writing to %s in pod %s/%s: %w", filePath, namespace, podName, lastErr)
}

// execReadFileFromPod reads a file from inside a running pod via kubectl exec.
func execReadFileFromPod(ctx context.Context, namespace, podName, filePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace, podName, "--",
		"cat", filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl exec read %s failed: %w (stderr: %s)", filePath, err, stderr.String())
	}
	return stdout.String(), nil
}

// execReadFileFromPodWithRetry retries reading a file from the pod, waiting for
// the container to become exec-able after a pod restart.
func execReadFileFromPodWithRetry(ctx context.Context, namespace, podName, filePath string) (string, error) {
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		content, err := execReadFileFromPod(ctx, namespace, podName, filePath)
		if err == nil {
			return content, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("timed out reading %s from pod %s/%s: %w", filePath, namespace, podName, lastErr)
}
