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

// Package rawpatch builds tiny, targeted JSON merge-patch payloads for
// metadata-only writes.
//
// Motivation: the DeepCopy+client.MergeFrom pattern serializes the ENTIRE
// object twice (base and modified) and diffs the two JSON documents just to
// emit a one- or two-key annotation or label patch. On the claim hot path
// that full-object serialize/diff was measured at double-digit percent of
// controller CPU in a 300-claim warm-adoption benchmark (the claim
// annotation write alone was 15.8%). The helpers here marshal ONLY the
// intended keys, producing byte-identical wire payloads to what MergeFrom
// would have computed for the same metadata-only mutation, at O(patch)
// instead of O(object) cost.
//
// Only additive set operations are supported deliberately: deleting a key via
// a merge patch requires an explicit JSON null, which these constructors do
// not emit (an empty-string value is still a set). Callers that need deletion
// keep using client.MergeFrom.
package rawpatch

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// metadataPatch is the shape of a metadata-only merge patch:
// {"metadata":{"annotations":{...}}} / {"metadata":{"labels":{...}}}.
type metadataPatch struct {
	Metadata metadataMaps `json:"metadata"`
}

type metadataMaps struct {
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// Annotations returns a merge patch that sets exactly the given annotation
// key/value pairs and touches nothing else. Values pass through
// encoding/json, so arbitrary strings (quotes, newlines, non-ASCII) are
// escaped exactly as client.MergeFrom would escape them; map keys are emitted
// in sorted order (encoding/json map ordering), matching MergeFrom's output
// byte for byte.
func Annotations(kv map[string]string) (client.Patch, error) {
	if len(kv) == 0 {
		return nil, fmt.Errorf("rawpatch.Annotations: empty key/value set")
	}
	data, err := json.Marshal(metadataPatch{Metadata: metadataMaps{Annotations: kv}})
	if err != nil {
		return nil, fmt.Errorf("rawpatch.Annotations: %w", err)
	}
	return client.RawPatch(types.MergePatchType, data), nil
}

// Labels returns a merge patch that sets exactly the given label key/value
// pairs and touches nothing else. See Annotations for encoding guarantees.
func Labels(kv map[string]string) (client.Patch, error) {
	if len(kv) == 0 {
		return nil, fmt.Errorf("rawpatch.Labels: empty key/value set")
	}
	data, err := json.Marshal(metadataPatch{Metadata: metadataMaps{Labels: kv}})
	if err != nil {
		return nil, fmt.Errorf("rawpatch.Labels: %w", err)
	}
	return client.RawPatch(types.MergePatchType, data), nil
}
