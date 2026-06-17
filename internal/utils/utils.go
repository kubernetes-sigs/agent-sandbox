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

package utils

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GetGroupKind returns the group and kind from the OwnerReference, or empty strings if ref is nil.
func GetGroupKind(ref *metav1.OwnerReference) (string, string) {
	if ref == nil {
		return "", ""
	}
	gvk := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
	return gvk.Group, gvk.Kind
}

// MatchesGroupKind returns true if the OwnerReference is not nil, and matches the specified group and kind.
func MatchesGroupKind(ref *metav1.OwnerReference, group, kind string) bool {
	g, k := GetGroupKind(ref)
	return g == group && k == kind
}
