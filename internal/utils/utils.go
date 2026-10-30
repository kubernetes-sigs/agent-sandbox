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
	"k8s.io/apimachinery/pkg/types"
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
	if ref == nil {
		return false
	}
	g, k := GetGroupKind(ref)
	return g == group && k == kind
}

// RefreshedOwnerReferences returns a copy of refs in which every reference to
// ownerUID uses apiVersion, and reports whether any reference changed. It
// does not modify refs, so callers can skip copying the object when nothing
// changes.
//
// Objects that a controller created before v1.0 keep owner references with
// apiVersion v1alpha1, because the storage migration rewrites each resource
// but not the references that other objects hold to it. After v1.0 stops
// serving v1alpha1, the garbage collector cannot resolve those owners, so it
// never deletes the dependents and foreground deletion of the owner hangs.
// The function only changes references in the same API group as apiVersion.
func RefreshedOwnerReferences(refs []metav1.OwnerReference, ownerUID types.UID, apiVersion string) ([]metav1.OwnerReference, bool) {
	group := schema.FromAPIVersionAndKind(apiVersion, "").Group
	var refreshed []metav1.OwnerReference
	for i, ref := range refs {
		if ref.UID != ownerUID || ref.APIVersion == apiVersion {
			continue
		}
		if schema.FromAPIVersionAndKind(ref.APIVersion, "").Group != group {
			continue
		}
		if refreshed == nil {
			refreshed = append([]metav1.OwnerReference(nil), refs...)
		}
		refreshed[i].APIVersion = apiVersion
	}
	return refreshed, refreshed != nil
}
