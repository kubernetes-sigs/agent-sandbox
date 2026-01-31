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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HasAnnotation verifies the object has the specified annotation
func HasAnnotation(key, wantVal string) ObjectPredicate {
	return func(obj client.Object) (bool, error) {
		if obj == nil {
			return false, fmt.Errorf("object is nil")
		}
		if got, ok := obj.GetAnnotations()[key]; !ok {
			return false, fmt.Errorf("annotation %s missing", key)
		} else if wantVal != got {
			return false, nil
		}
		return true, nil
	}
}

// HasLabel verifies the object has the specified label
func HasLabel(key, wantVal string) ObjectPredicate {
	return func(obj client.Object) (bool, error) {
		if obj == nil {
			return false, fmt.Errorf("object is nil")
		}
		if got, ok := obj.GetLabels()[key]; !ok {
			return false, fmt.Errorf("label %s missing", key)
		} else if wantVal != got {
			return false, nil
		}
		return true, nil
	}
}

// HasOwnerReferences verifies the object has the specified owner references
func HasOwnerReferences(want []metav1.OwnerReference) ObjectPredicate {
	return func(obj client.Object) (bool, error) {
		if obj == nil {
			return false, fmt.Errorf("object is nil")
		}
		opts := []cmp.Option{
			cmpopts.SortSlices(func(a, b metav1.OwnerReference) bool { return a.UID < b.UID }),
		}
		if diff := cmp.Diff(want, obj.GetOwnerReferences(), opts...); diff != "" {
			return false, nil
		}
		return true, nil
	}
}

// NotDeleted verifies the object has no deletion timestamp
func NotDeleted() ObjectPredicate {
	return func(obj client.Object) (bool, error) {
		if obj == nil {
			return false, fmt.Errorf("object is nil")
		}
		deletionTimestamp := obj.GetDeletionTimestamp()
		if deletionTimestamp != nil {
			return false, nil
		}
		return true, nil
	}
}
