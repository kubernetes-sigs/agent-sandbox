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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
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
	utilruntime.Must(extensionsv1beta1.AddToScheme(controllers.Scheme))
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

	// benchmark is populated if this is a benchmark
	benchmark *testing.B
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

	if b, ok := t.(*testing.B); ok {
		th.benchmark = b
	}

	kubeconfig := GetKubeconfig()
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	restConfig.QPS = 50
	restConfig.Burst = 100
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
		restConfig:    restConfig,
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

func (th *TestContext) dumpControllerLogs() {
	th.Helper()
	th.fetchControllerLogs(nil, "")
}

// DumpControllerLogsSince fetches controller logs from sinceTime onward and
// writes them to the artifacts directory with the given label in the filename.
func (th *TestContext) DumpControllerLogsSince(sinceTime time.Time, label string) {
	th.Helper()
	th.fetchControllerLogs(&sinceTime, label)
}

func (th *TestContext) fetchControllerLogs(sinceTime *time.Time, label string) {
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
		logOpts := &corev1.PodLogOptions{}
		if sinceTime != nil {
			since := metav1.NewTime(*sinceTime)
			logOpts.SinceTime = &since
		}

		stream, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, logOpts).Stream(context.Background())
		if err != nil {
			th.Logf("failed to get logs for pod %s: %v", pod.Name, err)
			continue
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(stream); err != nil {
			stream.Close()
			th.Logf("failed to read logs for pod %s: %v", pod.Name, err)
			continue
		}
		stream.Close()

		filename := fmt.Sprintf("controller-%s.log", pod.Name)
		if label != "" {
			filename = fmt.Sprintf("controller-%s-%s.log", label, pod.Name)
		}
		logFile := filepath.Join(th.artifactsDir, filename)
		if err := os.WriteFile(logFile, buf.Bytes(), 0o644); err != nil {
			th.Logf("failed to write controller logs to %s: %v", logFile, err)
		}

		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		tail := lines
		if len(tail) > 42 {
			tail = tail[len(tail)-42:]
		}
		th.Logf("=== Controller logs [%s] %s (%d lines, full: %s) ===\n%s",
			label, pod.Name, len(lines), logFile, strings.Join(tail, "\n"))
	}
}

// ReportMetric will report a benchmark result.
// If running as a benchmark it will be reported through the benchmark framework.
// If running as a unit-test it will be logged through the test.
func (th *TestContext) ReportMetric(n float64, unit string) {
	if th.benchmark != nil {
		th.benchmark.ReportMetric(n, unit)
	} else {
		th.Logf("benchmark[%v]=%v", unit, n)
	}
}

// StartTimer implements the same behavior as *testing.B.StartTimer, but for *framework.TestContext.
// It is ignored if we are not running as a benchmark.
func (th *TestContext) StartTimer() {
	if th.benchmark != nil {
		th.benchmark.StartTimer()
	}
}

// StopTimer implements the same behavior as *testing.B.StopTimer, but for *framework.TestContext.
// It is ignored if we are not running as a benchmark.
func (th *TestContext) StopTimer() {
	if th.benchmark != nil {
		th.benchmark.StopTimer()
	}
}
