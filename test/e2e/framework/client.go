// Copyright 2025 The Kubernetes Authors.
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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultTimeout is the default timeout for WaitForObject.
	DefaultTimeout = 60 * time.Second
)

// ClusterClient is an abstraction layer for test cases to interact with the cluster.
type ClusterClient struct {
	*testing.T
	client     client.Client
	restConfig *rest.Config
}

// Update an object that already exists on the cluster.
func (cl *ClusterClient) Update(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Updating object %T (%s)", obj, nn.String())
	if err := cl.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("update %T (%s): %w", obj, nn.String(), err)
	}
	return nil
}

// CreateWithCleanup creates the specified object and cleans up the object after
// the test completes.
func (cl *ClusterClient) CreateWithCleanup(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Creating object %T (%s)", obj, nn.String())
	if err := cl.client.Create(ctx, obj); err != nil {
		return fmt.Errorf("CreateWithCleanup %T (%s): %w", obj, nn.String(), err)
	}
	cl.Cleanup(func() {
		cl.Helper()
		cl.Logf("Deleting object %T (%s)", obj, nn.String())
		// Use context.Background because test context is done during cleanup
		if err := cl.client.Delete(context.Background(), obj); err != nil && !k8serrors.IsNotFound(err) {
			cl.Errorf("CreateWithCleanup %T (%s): %s", obj, nn.String(), err)
		}
		if err := cl.WaitForObjectNotFound(context.Background(), obj); err != nil {
			cl.Errorf("CreateWithCleanup %T (%s): %s", obj, nn.String(), err)
		}
	})
	return nil
}

// ValidateObject verifies the specified object exists and satisfies the provided
// predicates.
func (cl *ClusterClient) ValidateObject(ctx context.Context, obj client.Object, p ...predicates.ObjectPredicate) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("ValidateObject %T (%s)", obj, nn.String())
	if err := cl.client.Get(ctx, nn, obj); err != nil {
		return fmt.Errorf("ValidateObject %T (%s): %w", obj, nn.String(), err)
	}
	for _, predicate := range p {
		if err := predicate(obj); err != nil {
			return fmt.Errorf("ValidateObject %T (%s): %w", obj, nn.String(), err)
		}
	}
	return nil
}

// ValidateObjectNotFound verifies the specified object does not exist.
func (cl *ClusterClient) ValidateObjectNotFound(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("ValidateObjectNotFound %T (%s)", obj, nn.String())
	err := cl.client.Get(ctx, nn, obj)
	if err == nil { // object still exists - error
		return fmt.Errorf("ValidateObjectNotFound %T (%s): object still exists",
			obj, nn.String())
	} else if !k8serrors.IsNotFound(err) { // unexpected error
		return fmt.Errorf("ValidateObjectNotFound %T (%s): %w",
			obj, nn.String(), err)
	}
	return nil // happy path - object not found
}

// WaitForObject waits for the specified object to exist and satisfy the provided
// predicates.
func (cl *ClusterClient) WaitForObject(ctx context.Context, obj client.Object, p ...predicates.ObjectPredicate) error {
	cl.Helper()
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	start := time.Now()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	defer func() {
		cl.Helper()
		cl.Logf("WaitForObject %T (%s) took %s", obj, nn, time.Since(start))
	}()
	var validationErr error
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for object: %w", validationErr)
		default:
			if validationErr = cl.ValidateObject(ctx, obj, p...); validationErr == nil {
				return nil
			}
			// Simple sleep for fixed duration (basic MVP)
			time.Sleep(time.Second)
		}
	}
}

// WaitForObjectNotFound waits for the specified object to not exist.
func (cl *ClusterClient) WaitForObjectNotFound(ctx context.Context, obj client.Object) error {
	cl.Helper()
	// Static 30 second timeout, this can be adjusted if needed
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	defer func() {
		cl.Helper()
		cl.Logf("WaitForObjectNotFound %T (%s) took %s", obj, nn, time.Since(start))
	}()
	var validationErr error
	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timed out waiting for object: %w", validationErr)
		default:
			if validationErr = cl.ValidateObjectNotFound(timeoutCtx, obj); validationErr == nil {
				return nil
			}
			// Simple sleep for fixed duration (basic MVP)
			time.Sleep(time.Second)
		}
	}
}

// validateAgentSandboxInstallation verifies agent-sandbox system components are
// installed.
func (cl *ClusterClient) validateAgentSandboxInstallation(ctx context.Context) error {
	cl.Helper()
	// verify CRDs exist
	crds := []string{
		"sandboxes.agents.x-k8s.io",
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxtemplates.extensions.agents.x-k8s.io",
	}
	for _, name := range crds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = name
		if err := cl.ValidateObject(ctx, crd); err != nil {
			return fmt.Errorf("expected %T (%s) to exist: %w", crd, name, err)
		}
	}
	// verify agent-sandbox-system namespace exists
	ns := &corev1.Namespace{}
	ns.Name = "agent-sandbox-system"
	if err := cl.ValidateObject(ctx, ns); err != nil {
		return fmt.Errorf("expected %T (%s) to exist: %w", ns, ns.Name, err)
	}
	// verify agent-sandbox-controller exists
	ctrlNN := types.NamespacedName{
		Name:      "agent-sandbox-controller",
		Namespace: ns.Name,
	}
	ctrl := &appsv1.StatefulSet{}
	ctrl.Name = ctrlNN.Name
	ctrl.Namespace = ctrlNN.Namespace
	if err := cl.ValidateObject(ctx, ctrl); err != nil {
		return fmt.Errorf("expected %T (%s) to exist: %w", ctrl, ctrlNN.String(), err)
	}
	return nil
}

func (cl *ClusterClient) Apply(ctx context.Context, namespace string, manifest string) {
	tempDir := cl.T.TempDir()
	manifestPath := filepath.Join(tempDir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0644); err != nil {
		cl.T.Fatalf("failed to write manifest file %q: %v", manifestPath, err)
	}
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", manifestPath, "-n", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cl.T.Fatalf("failed to apply manifest %q: %v", manifestPath, err)
	}
}

var sandboxGVK = schema.GroupVersionKind{
	Group:   "agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "Sandbox",
}

var sandboxTemplateGVK = schema.GroupVersionKind{
	Group:   "extensions.agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "SandboxTemplate",
}

var sandboxWarmpoolGVK = schema.GroupVersionKind{
	Group:   "extensions.agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "SandboxWarmPool",
}

func (cl *ClusterClient) RESTConfig() *rest.Config {
	return cl.restConfig
}

func (cl *ClusterClient) CreateTempNamespace(ctx context.Context, name string) {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)

	if err := cl.CreateWithCleanup(ctx, ns); err != nil {
		cl.T.Fatalf("failed to create namespace %q: %v", name, err)
	}
}

func (cl *ClusterClient) PortForward(ctx context.Context, pod types.NamespacedName, localPort, remotePort int) {
	log := klog.FromContext(ctx)

	// Set up a port-forward to the Chrome Debug Port
	portForward := exec.CommandContext(ctx, "kubectl", "-n", pod.Namespace, "port-forward", "pod/"+pod.Name, fmt.Sprintf("%d:%d", localPort, remotePort))
	log.Info("starting port-forward", "command", portForward.String())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	portForward.Stdout = io.MultiWriter(os.Stdout, &stdout)
	portForward.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := portForward.Start(); err != nil {
		cl.T.Fatalf("failed to start port-forward: %v", err)
	}

	stopProcess := func() {
		if portForward.ProcessState != nil {
			if portForward.ProcessState.Exited() {
				return
			}
		}
		log.Info("killing port-forward")
		if err := portForward.Process.Kill(); err != nil {
			log.Error(err, "failed to kill port-forward")
		}
	}
	cl.T.Cleanup(stopProcess)

	go func() {
		if err := portForward.Wait(); err != nil {
			log.Error(err, "port-forward exited with error")
		} else {
			log.Info("port-forward exited")
		}
	}()

	// There is a delay after starting the process before it starts listening.
	// Wait for the "Forwarding from" message
	for {
		if portForward.ProcessState != nil {
			if portForward.ProcessState.Exited() {
				cl.T.Fatalf("port-forward process exited unexpectedly: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		}

		// Check stdout for the "Forwarding from" message
		if strings.Contains(stdout.String(), "Forwarding from") {
			log.Info("port-forward is ready", "stdout", stdout.String(), "stderr", stderr.String())
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (cl *ClusterClient) WaitForSandboxReady(ctx context.Context, sandboxID types.NamespacedName) error {
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	sandbox.SetName(sandboxID.Name)
	sandbox.SetNamespace(sandboxID.Namespace)

	if err := cl.WaitForObject(ctx, sandbox, predicates.ReadyConditionIsTrue); err != nil {
		cl.T.Fatalf("waiting for sandbox to be ready: %v", err)
		return err
	}
	return nil
}

func (cl *ClusterClient) WaitForWarmPoolReady(ctx context.Context, sandboxWarmpoolID types.NamespacedName, expectedReplicas int) error {
	cl.Helper()
	log := klog.FromContext(ctx)
	log.Info("Waiting for SandboxWarmPool Pods to be ready", "warmpoolID", sandboxWarmpoolID, "expectedReplicas", expectedReplicas)

	// Get the SandboxWarmPool object to access its UID
	warmpool := &unstructured.Unstructured{}
	warmpool.SetGroupVersionKind(sandboxWarmpoolGVK)
	if err := cl.client.Get(ctx, sandboxWarmpoolID, warmpool); err != nil {
		cl.T.Fatalf("Failed to get SandboxWarmPool %s: %v", sandboxWarmpoolID, err)
		return err
	}
	warmpoolUID := warmpool.GetUID()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 1*time.Minute, true, func(ctx context.Context) (bool, error) {
		podList := &corev1.PodList{}
		// List all pods in the namespace
		err := cl.client.List(ctx, podList, client.InNamespace(sandboxWarmpoolID.Namespace))
		if err != nil {
			log.Error(err, "Failed to list pods in namespace", "namespace", sandboxWarmpoolID.Namespace)
			return false, nil // Continue polling
		}

		readyCount := 0
		var foundPods []string
		for _, pod := range podList.Items {
			isOwned := false
			for _, ownerRef := range pod.GetOwnerReferences() {
				if ownerRef.Kind == "SandboxWarmPool" && ownerRef.UID == warmpoolUID {
					isOwned = true
					break
				}
			}

			if isOwned {
				foundPods = append(foundPods, pod.Name)
				if err := predicates.ReadyConditionIsTrue(&pod); err == nil {
					readyCount++
				}
			}
		}

		log.Info("WarmPool pod status", "warmpoolID", sandboxWarmpoolID, "foundOwned", len(foundPods), "ready", readyCount, "want", expectedReplicas, "pods", foundPods)

		if readyCount >= expectedReplicas {
			log.Info("SandboxWarmPool is ready", "warmpoolID", sandboxWarmpoolID)
			return true, nil // Done
		}
		return false, nil // Continue polling
	})

	if err != nil {
		cl.T.Fatalf("Timed out waiting for SandboxWarmPool %s to have %d ready pods: %v", sandboxWarmpoolID.String(), expectedReplicas, err)
		return err
	}
	return nil
}

func (cl *ClusterClient) GetSandboxTemplate(ctx context.Context, templateID types.NamespacedName) *unstructured.Unstructured {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(sandboxTemplateGVK)
	template.SetName(templateID.Name)
	template.SetNamespace(templateID.Namespace)

	if err := cl.client.Get(ctx, templateID, template); err != nil {
		cl.T.Fatalf("failed to get SandboxTemplate %s: %v", templateID, err)
	}
	return template
}

func (cl *ClusterClient) GetSandbox(ctx context.Context, sandboxID types.NamespacedName) *unstructured.Unstructured {
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	sandbox.SetName(sandboxID.Name)
	sandbox.SetNamespace(sandboxID.Namespace)

	if err := cl.client.Get(ctx, sandboxID, sandbox); err != nil {
		cl.T.Fatalf("failed to get Sandbox %s: %v", sandboxID, err)
	}
	return sandbox
}
