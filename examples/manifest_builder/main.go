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

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// BuildSandboxClaimResource constructs a dynamic SandboxClaim Custom Resource
// that references a pre-defined SandboxTemplate. This ensures the resulting
// Sandbox inherits all security and runtime configurations (like gVisor).
func BuildSandboxClaimResource(claimName, namespace, templateName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata": map[string]interface{}{
				"name":      claimName,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"agents.x-k8s.io/template-name": templateName,
				},
			},
			"spec": map[string]interface{}{
				"sandboxTemplateRef": map[string]interface{}{
					"name": templateName,
				},
			},
		},
	}
}

func main() {
	// 1. Setup Configuration
	templateName := "python-sandbox-template"
	claimName := "guide-verification-claim"
	namespace := "default"

	// 2. Initialize Kubernetes Dynamic Client
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("failed to build config: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create dynamic client: %v", err)
	}

	// 3. Define the GVR (Group, Version, Resource) for SandboxClaims
	gvr := schema.GroupVersionResource{
		Group:    "extensions.agents.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "sandboxclaims",
	}

	// 4. Construct the SandboxClaim
	claim := BuildSandboxClaimResource(claimName, namespace, templateName)

	// 5. Deploy the SandboxClaim to the cluster
	fmt.Printf("Deploying SandboxClaim '%s' referencing template '%s'...\n", claimName, templateName)
	_, err = dynClient.Resource(gvr).Namespace(namespace).Create(context.TODO(), claim, metav1.CreateOptions{})
	if err != nil {
		// If it already exists, try to update it or ignore
		log.Fatalf("failed to create SandboxClaim: %v", err)
	}

	fmt.Printf("Successfully created SandboxClaim: %s\n", claimName)
	fmt.Println("The agent-sandbox-controller will now provision a gRPC-enabled Sandbox for you.")
}
