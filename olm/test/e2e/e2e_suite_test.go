/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/agent-sandbox/olm/test/utils"
)

var (
	// controllerImage is built from the parent repo Dockerfile and loaded into Kind.
	controllerImage = "example.com/agent-sandbox-controller:e2e"
)

// TestE2E runs smoke tests that install the operator manifests and verify the
// upstream agent-sandbox-controller Deployment becomes ready.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting agent-sandbox-operator e2e suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the upstream agent-sandbox-controller image")
	// controller-image-build uses $(REPO_ROOT)/Dockerfile (parent repo), not a local Dockerfile.
	cmd := exec.Command("make", "controller-image-build", fmt.Sprintf("IMG=%s", controllerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the controller image")

	By("loading the controller image into Kind")
	err = utils.LoadImageToKindClusterWithName(controllerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the controller image into Kind")
})
