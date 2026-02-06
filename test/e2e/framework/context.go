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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// root directory of the agent-sandbox repository
	repoRoot = getRepoRoot()
	// The e2e tests use the context specified in the local KUBECONFIG file.
	// A localized KUBECONFIG is used to create an explicit cluster contract with
	// the tests.
	kubeconfig = filepath.Join(repoRoot, "bin", "KUBECONFIG")
)

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

// logCapturingT wraps a T and captures all log output to a file in addition
// to the normal test output. This allows per-test log files with timing info.
type logCapturingT struct {
	T
	file      *os.File
	mu        sync.Mutex
	startTime time.Time
}

// newLogCapturingT creates a new logCapturingT that writes logs to a file in artifactsDir.
func newLogCapturingT(t T, artifactsDir string) *logCapturingT {
	logFile := filepath.Join(artifactsDir, "test.log")
	f, err := os.Create(logFile)
	if err != nil {
		t.Logf("warning: failed to create test log file %s: %v", logFile, err)
		return &logCapturingT{T: t, startTime: time.Now()}
	}

	t.Cleanup(func() {
		f.Close()
	})

	lc := &logCapturingT{
		T:         t,
		file:      f,
		startTime: time.Now(),
	}
	lc.writeToFile("=== Test started: %s @%v ===\n", t.Name(), lc.startTime.UTC().Format("2006-01-02T15:04:05.000"))
	return lc
}

func (lc *logCapturingT) writeToFile(format string, args ...any) {
	if lc.file == nil {
		return
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()

	elapsed := time.Since(lc.startTime)
	timestamp := fmt.Sprintf("[%10.3fs] ", elapsed.Seconds())
	fmt.Fprintf(lc.file, timestamp+format, args...)
}

func (lc *logCapturingT) Log(args ...any) {
	lc.T.Helper()
	lc.T.Log(args...)
	lc.writeToFile("%s\n", fmt.Sprint(args...))
}

func (lc *logCapturingT) Logf(format string, args ...any) {
	lc.T.Helper()
	lc.T.Logf(format, args...)
	lc.writeToFile(format+"\n", args...)
}

func (lc *logCapturingT) Error(args ...any) {
	lc.T.Helper()
	lc.T.Error(args...)
	lc.writeToFile("ERROR: %s\n", fmt.Sprint(args...))
}

func (lc *logCapturingT) Errorf(format string, args ...any) {
	lc.T.Helper()
	lc.T.Errorf(format, args...)
	lc.writeToFile("ERROR: "+format+"\n", args...)
}

func (lc *logCapturingT) Fatal(args ...any) {
	lc.T.Helper()
	lc.writeToFile("FATAL: %s\n", fmt.Sprint(args...))
	lc.T.Fatal(args...)
}

func (lc *logCapturingT) Fatalf(format string, args ...any) {
	lc.T.Helper()
	lc.writeToFile("FATAL: "+format+"\n", args...)
	lc.T.Fatalf(format, args...)
}

// TestContext is a helper for managing e2e test scaffolding.
type TestContext struct {
	T
	*ClusterClient
	artifactsDir string
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
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}

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
		T:             wrappedT,
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
	return nil // currently no-op, add functionality as needed
}
