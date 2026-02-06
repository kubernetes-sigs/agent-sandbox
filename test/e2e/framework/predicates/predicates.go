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

import "sigs.k8s.io/controller-runtime/pkg/client"

// ObjectPredicate is a function that evaluates a predicate against a client.Object.
// When the predicate matches, it returns true.
// If there is an error during evaluation, it returns an error.
type ObjectPredicate func(obj client.Object) (bool, error)
