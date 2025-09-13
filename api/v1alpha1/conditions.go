/*
Copyright 2025.

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

package v1alpha1

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (
	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"
	// SandboxConditionPodReady indicates that the Sandbox Pod readniness
	SandboxConditionPodReady ConditionType = "PodReady"
	// SandboxConditionServiceReady indicates that the Sandbox Service readniness
	SandboxConditionServiceReady ConditionType = "ServiceReady"
)
