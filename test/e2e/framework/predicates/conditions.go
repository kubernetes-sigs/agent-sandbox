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

package predicates

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// objectWithStatus is a simplified struct to parse the status of a resource.
type objectWithStatus struct {
	Status struct {
		Conditions []metav1.Condition `json:"conditions,omitempty"`
	} `json:"status"`
}

// ReadyConditionIsTrue checks if the given object has a Ready condition set to True.
func ReadyConditionIsTrue(obj client.Object) error {
	u, err := asUnstructured(obj)
	if err != nil {
		return fmt.Errorf("failed to convert to unstructured: %w", err)
	}

	var status objectWithStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &status); err != nil {
		return fmt.Errorf("failed to convert to objectWithStatus: %v", err)
	}

	for _, cond := range status.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("object is not ready: %v", status.Status.Conditions)
}

// asUnstructured converts a client.Object to an *unstructured.Unstructured.
func asUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u, nil
	}

	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("converting object of type %T to unstructured: %w", obj, err)
	}
	return &unstructured.Unstructured{Object: m}, nil
}
