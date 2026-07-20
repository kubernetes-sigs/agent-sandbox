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

// Command mint-token mints a scoped sandbox-router token and prints it to
// stdout. Nothing more.
//
// This stands in for the Sandbox controller: in a real deployment, minting
// happens at Sandbox-creation time and the token is handed to the agent via
// whatever channel that design settles on (Sandbox status field,
// controller-managed Secret, etc. — an open follow-up, not this example).
// Here, the operator/test harness plays that role explicitly so the example
// can be run end-to-end today without waiting on that design.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
)

func main() {
	secretFile := flag.String("secret-file", "", "path to the file holding the shared HMAC secret (must match the router's --authz-scoped-token-secret-file)")
	namespace := flag.String("namespace", "default", "namespace the token is scoped to")
	name := flag.String("name", "", "Sandbox name the token is scoped to (must match X-Sandbox-ID)")
	ttl := flag.Duration("ttl", 10*time.Minute, "how long the token is valid for")
	flag.Parse()

	if *secretFile == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: mint-token --secret-file=<path> --namespace=<ns> --name=<sandbox> [--ttl=10m]")
		os.Exit(2)
	}

	secret, err := os.ReadFile(*secretFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read secret file: %v\n", err)
		os.Exit(1)
	}

	token, err := authz.MintScopedToken(bytes.TrimSpace(secret), *namespace, *name, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(token)
}
