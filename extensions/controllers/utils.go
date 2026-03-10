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

package controllers

import (
	"encoding/json"

	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	// poolLabel is the label used to identify pods belonging to a warm pool.
	poolLabel = "agents.x-k8s.io/pool"

	// sandboxTemplateRefHash tracks the name-hash of the template used.
	sandboxTemplateRefHash = "agents.x-k8s.io/sandbox-template-ref-hash"

	// sandboxTemplateHash tracks the specific version/content of the template.
	sandboxTemplateHash = "agents.x-k8s.io/sandbox-template-hash"

	// templateRefField is the field used for indexing SandboxWarmPools by their template reference name.
	templateRefField = ".spec.templateRef.Name"

	// SandboxWarmPool update strategies
	RecreateStrategy = "Recreate"
	OnDeleteStrategy = "OnDelete"
)

// computeTemplateHash computes a hash of the sandbox template spec and its name.
func computeTemplateHash(template *extensionsv1alpha1.SandboxTemplate) string {
	// Include the name in the hash to differentiate templates with identical specs.
	data := struct {
		Name string                                 `json:"name"`
		Spec extensionsv1alpha1.SandboxTemplateSpec `json:"spec"`
	}{
		Name: template.Name,
		Spec: template.Spec,
	}
	specJSON, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return sandboxcontrollers.NameHash(string(specJSON))
}
