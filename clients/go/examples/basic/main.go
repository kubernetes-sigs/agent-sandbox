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

// basic demonstrates minimal usage of the Go sandbox client in dev mode
// (port-forward to sandbox-router-svc).
package main

import (
	"context"
	"fmt"
	"log"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

func main() {
	client, err := sandbox.New(context.Background(), sandbox.Options{
		TemplateName: "my-sandbox-template",
		Namespace:    "default",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close(context.Background())

	ctx := context.Background()
	if err := client.Open(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Sandbox ready: claim=%s pod=%s\n", client.ClaimName(), client.PodName())

	result, err := client.Run(ctx, "echo 'Hello from Go!'")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stdout: %s", result.Stdout)
	fmt.Printf("exit_code: %d\n", result.ExitCode)

	if err := client.Write(ctx, "hello.txt", []byte("Hello, world!")); err != nil {
		log.Fatal(err)
	}

	data, err := client.Read(ctx, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("file content: %s\n", string(data))

	entries, err := client.List(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		fmt.Printf("  %s\t%s\t%d bytes\n", e.Type, e.Name, e.Size)
	}
}
