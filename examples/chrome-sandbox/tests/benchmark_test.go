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

package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type Harness struct {
	t *testing.T
}

func NewHarness(t *testing.T) *Harness {
	return &Harness{t: t}
}

func (h *Harness) ApplyManifest(ctx context.Context, namespace string, manifestPath string) {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", manifestPath, "-n", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("failed to apply manifest %q: %v", manifestPath, err)
	}
}

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

func (h *Harness) RESTConfig() *rest.Config {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		h.t.Fatalf("failed to load kubeconfig: %v", err)
	}
	if config.QPS == 0.0 {
		// Disable client-side ratelimer by default, we can rely on
		// API priority and fairness
		config.QPS = -1
	}
	return config

}

func (h *Harness) DynamicClient() dynamic.Interface {
	restConfig := h.RESTConfig()

	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatalf("failed to create dynamic client: %v", err)
	}
	return client
}

func (h *Harness) CreateTempNamespace(ctx context.Context, name string) {
	client := h.DynamicClient()

	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)

	gvr := schema.GroupVersionResource{
		Version:  "v1",
		Resource: "namespaces",
	}
	_, err := client.Resource(gvr).Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatalf("failed to create namespace %q: %v", name, err)
	}

	h.t.Cleanup(func() {
		ctx := context.Background()
		if err := client.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			h.t.Fatalf("failed to delete namespace %q: %v", name, err)
		}
	})
}
func TestRunChromeSandbox(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := klog.FromContext(ctx)

	startTime := time.Now()

	h := NewHarness(t)

	ns := fmt.Sprintf("chrome-sandbox-test-%d", time.Now().UnixNano())
	h.CreateTempNamespace(ctx, ns)

	h.ApplyManifest(ctx, ns, "../k8s/manifest.yaml")

	sandboxName := "chrome-sandbox" // TODO
	var sandbox *unstructured.Unstructured
	for {
		u, err := h.DynamicClient().Resource(sandboxGVR).Namespace(ns).Get(ctx, sandboxName, metav1.GetOptions{})
		if err != nil {
			h.t.Fatalf("getting sandbox failed: %v", err)
		}
		sandbox = u

		var status sandboxWithStatus
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sandbox.Object, &status); err != nil {
			t.Fatalf("failed to convert sandbox status: %v", err)
		}

		ready := false
		for _, cond := range status.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			break
		}
		log.Info("sandbox is not ready", "status", status)
		time.Sleep(100 * time.Millisecond)
	}

	podName := "chrome-sandbox" // TODO

	time.Sleep(1 * time.Second) // TODO: wait a bit for the pod to be ready

	portForward := exec.CommandContext(ctx, "kubectl", "-n", ns, "port-forward", "pod/"+podName, "9222:9222")
	log.Info("starting port-forward", "command", portForward.String())
	portForward.Stdout = os.Stdout
	portForward.Stderr = os.Stderr
	if err := portForward.Start(); err != nil {
		t.Fatalf("failed to start port-forward: %v", err)
	}

	go func() {
		if err := portForward.Wait(); err != nil {
			log.Error(err, "port-forward exited with error")
		} else {
			log.Info("port-forward exited")
		}
		cancel()
	}()

	t.Cleanup(func() {
		log.Info("killing port-forward")
		if err := portForward.Process.Kill(); err != nil {
			t.Fatalf("failed to kill port-forward: %v", err)
		}
	})

	for {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled")
		}
		u := fmt.Sprintf("http://localhost:9222/json/version")
		info, err := getChromeInfo(ctx, u)
		if err != nil {
			log.Error(err, "failed to get Chrome info")
			time.Sleep(100 * time.Millisecond)
			continue
		}
		log.Info("Chrome is ready", "url", u, "response", info)
		break
	}

	duration := time.Since(startTime)
	log.Info("Test completed successfully", "duration", duration)
}

func getChromeInfo(ctx context.Context, u string) (string, error) {

	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Send the HTTP request
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error sending HTTP request to Chrome Debug Port: %w", err)
	}
	defer response.Body.Close()

	// Check for HTTP 200 OK
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("non-200 response from Chrome Debug Port: %d", response.StatusCode)
	}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body from Chrome Debug Port: %w", err)
	}

	return string(b), nil
}

type sandboxWithStatus struct {
	Status struct {
		Conditions []metav1.Condition `json:"conditions,omitempty"`
	} `json:"status,omitempty"`
}
