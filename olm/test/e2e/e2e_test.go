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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/agent-sandbox/olm/test/utils"
)

const (
	namespace        = "agent-sandbox-system"
	deploymentName   = "agent-sandbox-controller"
	metricsService   = "agent-sandbox-controller"
	podLabelSelector = "app=agent-sandbox-controller"
)

var _ = Describe("Operator manifests", Ordered, func() {
	var controllerPodName string

	BeforeAll(func() {
		By("installing CRDs")
		cmd := exec.Command("make", "install")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", controllerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller")
	})

	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() || controllerPodName == "" {
			return
		}
		By("fetching controller pod logs after failure")
		cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
		if output, err := utils.Run(cmd); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", output)
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should install all operator CRDs", func() {
		crds := []string{
			"sandboxes.agents.x-k8s.io",
			"sandboxclaims.extensions.agents.x-k8s.io",
			"sandboxwarmpools.extensions.agents.x-k8s.io",
			"sandboxtemplates.extensions.agents.x-k8s.io",
		}
		for _, crd := range crds {
			cmd := exec.Command("kubectl", "get", "crd", crd)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "CRD %s should be installed", crd)
		}
	})

	It("should run the controller Deployment", func() {
		verifyControllerUp := func(g Gomega) {
			cmd := exec.Command("kubectl", "get",
				"pods", "-l", podLabelSelector,
				"-o", "go-template={{ range .items }}"+
					"{{ if not .metadata.deletionTimestamp }}"+
					"{{ .metadata.name }}"+
					"{{ \"\\n\" }}{{ end }}{{ end }}",
				"-n", namespace,
			)
			podOutput, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			podNames := utils.GetNonEmptyLines(podOutput)
			g.Expect(podNames).To(HaveLen(1))
			controllerPodName = podNames[0]

			cmd = exec.Command("kubectl", "get", "deployment", deploymentName,
				"-o", "jsonpath={.status.readyReplicas}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("1"))
		}
		Eventually(verifyControllerUp).Should(Succeed())
	})

	It("should serve HTTP metrics on port 8080", func() {
		By("waiting for the metrics Service endpoints")
		verifyEndpoints := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints", metricsService, "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("8080"))
		}
		Eventually(verifyEndpoints).Should(Succeed())

		By("curling the metrics endpoint from a short-lived pod")
		metricsURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/metrics", metricsService, namespace)
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found"))
		cmd := exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
			"--namespace", namespace,
			"--image=curlimages/curl:8.8.0",
			"--command", "--", "/bin/sh", "-c",
			fmt.Sprintf("curl -sf %s | grep -q controller_runtime", metricsURL),
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		verifyCurlSucceeded := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
				"-o", "jsonpath={.status.phase}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Succeeded"))
		}
		Eventually(verifyCurlSucceeded, 5*time.Minute).Should(Succeed())
	})
})
