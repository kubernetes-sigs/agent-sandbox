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
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetKubeconfig returns the path to the kubeconfig file used by the tests.
func GetKubeconfig() string {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		return kubeconfig
	}

	// root directory of the agent-sandbox repository.
	repoRoot := getRepoRoot()
	// The e2e tests use the context specified in the local KUBECONFIG file.
	// A localized KUBECONFIG is used to create an explicit cluster contract with
	// the tests.
	kubeconfig = filepath.Join(repoRoot, "bin", "KUBECONFIG")

	return kubeconfig
}

func init() {
	utilruntime.Must(apiextensionsv1.AddToScheme(controllers.Scheme))
	utilruntime.Must(extensionsv1alpha1.AddToScheme(controllers.Scheme))
}

func getRepoRoot() string {
	// This file is at <repo>/test/e2e/framework/context.go, so 3 Dir() hops (framework -> e2e -> test -> repo)
	// gives us the repository root regardless of the test package working directory.
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	return filepath.Dir(filepath.Dir(filepath.Dir(dir)))
}

// T extends testing.TB with the Context method available on T and B.
// Both *testing.T and *testing.B satisfy this interface.
type T interface {
	testing.TB
	Context() context.Context
}

// TestContext is a helper for managing e2e test scaffolding.
type TestContext struct {
	T
	*ClusterClient
	artifactsDir string
	restConfig   *rest.Config
}

// ArtifactsDir returns the directory where test artifacts should be written.
func (th *TestContext) ArtifactsDir() string {
	return th.artifactsDir
}

// NewTestContext creates a new TestContext. This should be called at the beginning
// of each e2e test to construct needed test scaffolding.
func NewTestContext(t T) *TestContext {
	t.Helper()

	// Set up artifacts directory for this test
	artifactsDir := os.Getenv("ARTIFACTS")
	if artifactsDir == "" {
		artifactsDir = "./artifacts"
	}
	artifactsDir = filepath.Join(artifactsDir, t.Name())
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("failed to create artifacts dir: %v", err)
	}

	// Wrap T with log capturing
	wrappedT := newLogCapturingT(t, artifactsDir)

	th := &TestContext{
		T:            wrappedT,
		artifactsDir: artifactsDir,
	}
	kubeconfig := GetKubeconfig()
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	th.restConfig = restConfig

	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		t.Fatalf("building HTTP client for rest config: %v", err)
	}

	client, err := client.New(restConfig, client.Options{
		Scheme:     controllers.Scheme,
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("building controller-runtime client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfigAndClient(restConfig, httpClient)
	if err != nil {
		t.Fatalf("building dynamic client: %v", err)
	}

	watchSet := NewWatchSet(dynamicClient)
	t.Cleanup(func() {
		watchSet.Close()
	})

	th.ClusterClient = &ClusterClient{
		T:             t,
		client:        client,
		dynamicClient: dynamicClient,
		scheme:        controllers.Scheme,
		watchSet:      watchSet,
	}
	t.Cleanup(func() {
		t.Helper()
		if err := th.afterEach(); err != nil {
			t.Error(err)
		}
	})
	if err := th.beforeEach(); err != nil {
		t.Fatal(err)
	}
	return th
}

// beforeEach runs before each test case is executed.
func (th *TestContext) beforeEach() error {
	th.Helper()
	return th.validateAgentSandboxInstallation()
}

// afterEach runs after each test case is executed.
//
//nolint:unparam // remove nolint once this is implemented
func (th *TestContext) afterEach() error {
	th.Helper()
	if th.Failed() {
		th.dumpControllerLogs()
	}
	return nil
}

// dumpControllerLogs fetches and logs the agent-sandbox-controller logs
// to help diagnose test failures.
func (th *TestContext) dumpControllerLogs() {
	th.Helper()

	clientset, err := kubernetes.NewForConfig(th.restConfig)
	if err != nil {
		th.Logf("failed to create clientset for controller logs: %v", err)
		return
	}

	pods, err := clientset.CoreV1().Pods("agent-sandbox-system").List(
		context.Background(),
		metav1.ListOptions{LabelSelector: "app=agent-sandbox-controller"},
	)
	if err != nil {
		th.Logf("failed to list controller pods: %v", err)
		return
	}

	for _, pod := range pods.Items {
		// Write full logs to artifacts file
		fullReq := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
		fullStream, err := fullReq.Stream(context.Background())
		if err != nil {
			th.Logf("failed to get logs for pod %s: %v", pod.Name, err)
			continue
		}
		var fullBuf bytes.Buffer
		if _, err := fullBuf.ReadFrom(fullStream); err != nil {
			fullStream.Close()
			th.Logf("failed to read logs for pod %s: %v", pod.Name, err)
			continue
		}
		fullStream.Close()

		logFile := filepath.Join(th.artifactsDir, fmt.Sprintf("controller-%s.log", pod.Name))
		if err := os.WriteFile(logFile, fullBuf.Bytes(), 0o644); err != nil {
			th.Logf("failed to write controller logs to %s: %v", logFile, err)
		}

		// Print last 42 lines to test output (following k8s e2e convention)
		tailReq := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: new(int64(42)),
		})
		tailStream, err := tailReq.Stream(context.Background())
		if err != nil {
			th.Logf("failed to get tail logs for pod %s: %v", pod.Name, err)
			continue
		}
		var tailBuf bytes.Buffer
		if _, err := tailBuf.ReadFrom(tailStream); err != nil {
			tailStream.Close()
			th.Logf("failed to read tail logs for pod %s: %v", pod.Name, err)
			continue
		}
		tailStream.Close()

		th.Logf("=== Controller logs (last 42 lines) from %s (full logs: %s) ===\n%s", pod.Name, logFile, tailBuf.String())
	}
}
