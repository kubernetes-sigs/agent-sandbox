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

package v1beta1_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

const serviceNameValidationMessage = "metadata.name must be a valid RFC 1035 label when spec.service is true"

const celPerCallLimit = 1_000_000

func TestSandboxServiceNameCELValidation(t *testing.T) {
	validator, schema := sandboxCELValidator(t, "v1beta1")

	tests := []struct {
		name      string
		object    map[string]any
		oldObject map[string]any
		wantError bool
	}{
		{
			name:   "valid Service name on create",
			object: sandboxObject("valid-name", new(true), nil),
		},
		{
			name:      "name longer than 63 characters on create",
			object:    sandboxObject("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl", new(true), nil),
			wantError: true,
		},
		{
			name:      "name starting with a digit on create",
			object:    sandboxObject("0abc", new(true), nil),
			wantError: true,
		},
		{
			name:      "name containing a dot on create",
			object:    sandboxObject("a.b", new(true), nil),
			wantError: true,
		},
		{
			name:   "invalid Service name when service is disabled",
			object: sandboxObject("a.b", new(false), nil),
		},
		{
			name:      "enabling service for an invalid name",
			oldObject: sandboxObject("a.b", new(false), nil),
			object:    sandboxObject("a.b", new(true), nil),
			wantError: true,
		},
		{
			name:      "status update for a legacy long Service name",
			oldObject: sandboxObject("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl", new(true), map[string]any{"service": "old"}),
			object:    sandboxObject("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl", new(true), map[string]any{"service": "updated"}),
		},
		{
			name:      "status update for a legacy malformed Service name",
			oldObject: sandboxObject("a.b", new(true), map[string]any{"service": "old"}),
			object:    sandboxObject("a.b", new(true), map[string]any{"service": "updated"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), field.NewPath("sandbox"), schema, tt.object, tt.oldObject, math.MaxInt)
			if tt.wantError {
				require.ErrorContains(t, errs.ToAggregate(), serviceNameValidationMessage)
				return
			}
			require.Empty(t, errs)
		})
	}
}

func sandboxCELValidator(t *testing.T, versionName string) (*celvalidation.Validator, *structuralschema.Structural) {
	t.Helper()

	crdPath := filepath.Join("..", "..", "k8s", "crds", "agents.x-k8s.io_sandboxes.yaml")
	data, err := os.ReadFile(crdPath)
	require.NoError(t, err)

	crd := &apiextensionsv1.CustomResourceDefinition{}
	require.NoError(t, yaml.Unmarshal(data, crd))

	var versionSchema *apiextensionsv1.JSONSchemaProps
	for _, version := range crd.Spec.Versions {
		if version.Name == versionName {
			versionSchema = version.Schema.OpenAPIV3Schema
			break
		}
	}
	require.NotNil(t, versionSchema)

	internalSchema := &apiextensions.JSONSchemaProps{}
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(versionSchema, internalSchema, nil))

	structural, err := structuralschema.NewStructural(internalSchema)
	require.NoError(t, err)

	validator := celvalidation.NewValidator(structural, true, celPerCallLimit)
	require.NotNil(t, validator)
	return validator, structural
}

func sandboxObject(name string, service *bool, status map[string]any) map[string]any {
	spec := map[string]any{
		"podTemplate": map[string]any{},
	}
	if service != nil {
		spec["service"] = *service
	}

	object := map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     spec,
	}
	if status != nil {
		object["status"] = status
	}
	return object
}
