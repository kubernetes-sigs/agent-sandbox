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

// Command metrics-docs-gen renders the controller metrics reference from the
// Prometheus metric definitions of a Go package.
package main

import (
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/agent-sandbox/internal/metricsdocs"
)

func main() {
	source := flag.String("source", "internal/metrics", "Directory of the Go package declaring the Prometheus metrics. Only files directly in it are read; subdirectories are other packages.")
	output := flag.String("output", "docs/metrics.md", "Path of the Markdown file to write.")
	flag.Parse()

	if err := metricsdocs.Generate(*source, *output); err != nil {
		fmt.Fprintf(os.Stderr, "metrics-docs-gen: %v\n", err)
		os.Exit(1)
	}
}
