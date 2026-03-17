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

// gateway demonstrates production usage of the Go sandbox client with
// Gateway API discovery for the sandbox-router external address.
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	client, err := sandbox.NewClient(sandbox.Options{
		TemplateName:     "my-sandbox-template",
		Namespace:        "default",
		GatewayName:      "external-http-gateway",
		GatewayNamespace: "default",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close(context.Background())

	ctx := context.Background()
	if err := client.Open(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Sandbox ready via gateway: claim=%s pod=%s\n", client.ClaimName(), client.PodName())

	result, err := client.Run(ctx, "python3 -c \"print('Hello from Python in Go!')\"")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stdout: %s", result.Stdout)
	fmt.Printf("exit_code: %d\n", result.ExitCode)
}
