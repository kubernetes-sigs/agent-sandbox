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

package runtimeadoption

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// PatchStatus writes only the claim-controller-owned field. A merge patch can
// create an absent status object without replacing another writer's fields;
// UID immutability and resourceVersion enforce identity and version together.
func PatchStatus(ctx context.Context, writer client.Client, object client.Object, value any) error {
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return errors.New("runtime adoption writes require UID and resourceVersion")
	}
	data, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": object.GetUID(), "resourceVersion": object.GetResourceVersion()},
		"status":   map[string]any{"runtimeAdoption": value},
	})
	if err != nil {
		return err
	}
	return writer.Status().Patch(ctx, object, client.RawPatch(types.MergePatchType, data))
}

func SetFinalizer(ctx context.Context, writer client.Client, object client.Object, present bool) error {
	finalizers := slices.Clone(object.GetFinalizers())
	contains := slices.Contains(finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
	if contains == present {
		return nil
	}
	if present {
		finalizers = append(finalizers, sandboxv1beta1.RuntimeAdoptionFinalizer)
	} else {
		finalizers = slices.DeleteFunc(finalizers, func(value string) bool { return value == sandboxv1beta1.RuntimeAdoptionFinalizer })
	}
	patch, err := guardedPatch(object, "/metadata/finalizers", finalizers)
	if err != nil {
		return err
	}
	return writer.Patch(ctx, object, patch)
}

func guardedPatch(object client.Object, path string, value any) (client.Patch, error) {
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return nil, errors.New("runtime adoption writes require UID and resourceVersion")
	}
	operations := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": object.GetUID()},
		{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
		{"op": "add", "path": path, "value": value},
	}
	data, err := json.Marshal(operations)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.JSONPatchType, data), nil
}
