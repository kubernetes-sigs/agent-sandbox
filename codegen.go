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

// This file just exists as a place to put //go:generate directives that should apply to the entire project

package agentsandbox

// Generate CRDs and RBAC rules
//go:generate go tool -modfile=tools.mod sigs.k8s.io/controller-tools/cmd/controller-gen object crd:maxDescLen=0 paths=./api/... output:crd:dir=k8s/crds
//go:generate go tool -modfile=tools.mod sigs.k8s.io/controller-tools/cmd/controller-gen object crd:maxDescLen=0 paths=./extensions/... output:crd:dir=k8s/crds
//go:generate go tool -modfile=tools.mod sigs.k8s.io/controller-tools/cmd/controller-gen paths=./controllers/... output:rbac:dir=k8s rbac:roleName=agent-sandbox-controller,fileName=rbac.generated.yaml
//go:generate go tool -modfile=tools.mod sigs.k8s.io/controller-tools/cmd/controller-gen paths=./extensions/controllers/... output:rbac:dir=k8s rbac:roleName=agent-sandbox-controller-extensions,fileName=extensions-rbac.generated.yaml
