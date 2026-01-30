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
	"path/filepath"
	"runtime"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

// TestContext is a helper for managing e2e test scaffolding.
type TestContext struct {
	T
	ClusterClient
}

// NewTestContext creates a new TestContext. This should be called at the beginning
// of each e2e test to construct needed test scaffolding.
func NewTestContext(t T) *TestContext {
	t.Helper()
	th := &TestContext{
		T: t,
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(restCfg, client.Options{
		Scheme: controllers.Scheme,
	})
	if err != nil {
		t.Fatal(err)
	}
	th.ClusterClient = ClusterClient{
		T:      t,
		client: cl,
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
