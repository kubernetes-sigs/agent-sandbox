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

/*
This program demonstrates client-side stochastic routing in Go.
It reads the canary-routing-config ConfigMap to determine which SandboxWarmPool to target
when creating a SandboxClaim. This allows for canary rollouts controlled by GitOps (Argo CD).
*/
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	extensionsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run main.go <claim_name>")
	}
	claimName := os.Args[1]

	ctx := context.Background()

	// Load Kubernetes config. It will try in-cluster config first (when running inside a pod),
	// and fall back to local kubeconfig (~/.kube/config) for local development.
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			log.Fatalf("Failed to load kubeconfig: %v", err)
		}
	}

	// Create standard Kubernetes clientset. We use this to read standard resources like ConfigMaps.
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	// Create Extensions clientset. This is generated code specific to agent-sandbox CRDs.
	// We use it to interact with SandboxClaims, SandboxWarmPools, etc.
	extClient, err := extensionsclientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create extensions client: %v", err)
	}

	// 1. Read routing config from ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, "canary-routing-config", metav1.GetOptions{})
	if err != nil {
		log.Fatalf("Failed to read ConfigMap: %v", err)
	}

	primaryPool := cm.Data["primary_pool"]
	if primaryPool == "" {
		primaryPool = "python-pool-v1"
	}
	canaryPool := cm.Data["canary_pool"]
	if canaryPool == "" {
		canaryPool = "python-pool-v2"
	}
	primaryTemplate := cm.Data["primary_template"]
	if primaryTemplate == "" {
		primaryTemplate = "sandbox-python-template-v1"
	}
	canaryTemplate := cm.Data["canary_template"]
	if canaryTemplate == "" {
		canaryTemplate = "sandbox-python-template-v2"
	}
	canaryPercentage := 0
	if _, err := fmt.Sscanf(cm.Data["canary_percentage"], "%d", &canaryPercentage); err != nil {
		log.Fatalf("Failed to parse canary_percentage: %v", err)
	}

	if canaryPercentage < 0 || canaryPercentage > 100 {
		log.Fatalf("Invalid canary percentage: %d. Must be between 0 and 100.", canaryPercentage)
	}

	fmt.Printf("Routing Config: Primary=%s, Canary=%s, Percentage=%d%%\n", primaryPool, canaryPool, canaryPercentage)

	// 2. Stochastic routing decision
	selectedPool := primaryPool
	selectedTemplate := primaryTemplate

	// Generate a random number between 0 and 99.
	// If it's less than the canary percentage, we route to the canary pool.
	if rand.Intn(100) < canaryPercentage {
		selectedPool = canaryPool
		selectedTemplate = canaryTemplate
		fmt.Println("[CANARY] Routing to Canary pool")
	} else {
		fmt.Println("[PRIMARY] Routing to Stable pool")
	}

	// 3. Create SandboxClaim using the selected pool

	warmPoolPolicy := extv1alpha1.WarmPoolPolicy(selectedPool)

	claim := &extv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			Labels: map[string]string{
				"app": "argocd-sdk-test", // Label used by analysis job to find these claims
			},
		},
		Spec: extv1alpha1.SandboxClaimSpec{
			TemplateRef: extv1alpha1.SandboxTemplateRef{
				Name: selectedTemplate, // Template defining the sandbox environment
			},
			WarmPool: &warmPoolPolicy, // Here we specify which pool to use
		},
	}

	createdClaim, err := extClient.ExtensionsV1alpha1().SandboxClaims("default").Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create SandboxClaim: %v", err)
	}

	fmt.Printf("Successfully created SandboxClaim %s targeting pool %s\n", createdClaim.Name, selectedPool)
}
