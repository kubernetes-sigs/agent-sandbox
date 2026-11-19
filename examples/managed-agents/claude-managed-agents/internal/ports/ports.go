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

// Package ports holds the in-sandbox port numbers shared by the controller
// and the runner so the two binaries cannot drift apart.
package ports

// RunnerHTTP is the port the runner's HTTP server listens on inside the
// sandbox pod. It matches containerPort in template.yaml.
const RunnerHTTP = 8080

// RunnerGRPCPlaceholder is the idle TCP listener the runner keeps so the Go
// SDK's pod tunnel can establish: the tunnel forwards both sandboxd
// data-plane ports and the SDK requires the two to differ, while the runner
// itself only serves HTTP.
const RunnerGRPCPlaceholder = 9090
