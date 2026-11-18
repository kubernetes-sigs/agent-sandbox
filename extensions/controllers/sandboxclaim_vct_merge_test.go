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

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// vct builds a volume claim template; storageClass tells same-named template and claim entries apart.
func vct(name, storageClass string) sandboxv1beta1.PersistentVolumeClaimTemplate {
	return sandboxv1beta1.PersistentVolumeClaimTemplate{
		EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: name},
		Spec:                   corev1.PersistentVolumeClaimSpec{StorageClassName: &storageClass},
	}
}

func vcts(v ...sandboxv1beta1.PersistentVolumeClaimTemplate) []sandboxv1beta1.PersistentVolumeClaimTemplate {
	return v
}

func TestMergeVolumeClaimTemplates(t *testing.T) {
	tests := []struct {
		name        string
		template    []sandboxv1beta1.PersistentVolumeClaimTemplate
		claim       []sandboxv1beta1.PersistentVolumeClaimTemplate
		policy      extensionsv1beta1.VolumeClaimTemplatesPolicy
		want        []sandboxv1beta1.PersistentVolumeClaimTemplate
		wantErr     error
		wantErrText string
	}{
		{
			name:     "claim without volumes keeps template volumes even when disallowed",
			template: vcts(vct("data", "tmpl")),
			policy:   extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			want:     vcts(vct("data", "tmpl")),
		},
		{
			name:     "disallowed policy rejects claim volumes",
			template: vcts(vct("data", "tmpl")),
			claim:    vcts(vct("cache", "claim")),
			policy:   extensionsv1beta1.VolumeClaimTemplatesPolicyDisallowed,
			wantErr:  ErrVolumeClaimTemplatesDisallowed,
		},
		{
			name:     "empty policy defaults to disallowed",
			template: vcts(vct("data", "tmpl")),
			claim:    vcts(vct("cache", "claim")),
			policy:   "",
			wantErr:  ErrVolumeClaimTemplatesDisallowed,
		},
		{
			name:     "allowed policy appends new claim volumes after template volumes",
			template: vcts(vct("data", "tmpl")),
			claim:    vcts(vct("cache", "claim"), vct("scratch", "claim")),
			policy:   extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			want:     vcts(vct("data", "tmpl"), vct("cache", "claim"), vct("scratch", "claim")),
		},
		{
			name:        "allowed policy forbids overriding a template volume by name",
			template:    vcts(vct("data", "tmpl")),
			claim:       vcts(vct("cache", "claim"), vct("data", "claim")),
			policy:      extensionsv1beta1.VolumeClaimTemplatesPolicyAllowed,
			wantErr:     ErrVolumeClaimTemplatesOverrideForbidden,
			wantErrText: `cannot override template volume "data"`,
		},
		{
			name:     "overrides policy replaces by name in template order and appends new names",
			template: vcts(vct("a", "tmpl"), vct("b", "tmpl"), vct("c", "tmpl")),
			claim:    vcts(vct("e", "claim"), vct("b", "claim"), vct("d", "claim")),
			policy:   extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			want:     vcts(vct("a", "tmpl"), vct("b", "claim"), vct("c", "tmpl"), vct("e", "claim"), vct("d", "claim")),
		},
		{
			name:        "unknown policy is rejected",
			template:    vcts(vct("data", "tmpl")),
			claim:       vcts(vct("cache", "claim")),
			policy:      "Sometimes",
			wantErrText: `unknown volume claim templates policy "Sometimes"`,
		},
		{
			name:        "invalid template volumes are reported with template prefix",
			template:    vcts(vct("data", "tmpl"), vct("data", "tmpl")),
			policy:      extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			wantErr:     ErrVolumeClaimTemplatesInvalid,
			wantErrText: "template: ",
		},
		{
			name:        "invalid claim volumes are reported with claim prefix",
			template:    vcts(vct("data", "tmpl")),
			claim:       vcts(vct("", "claim")),
			policy:      extensionsv1beta1.VolumeClaimTemplatesPolicyOverrides,
			wantErr:     ErrVolumeClaimTemplatesInvalid,
			wantErrText: "claim: ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeVolumeClaimTemplates(tc.template, tc.claim, tc.policy)
			if tc.wantErr == nil && tc.wantErrText == "" {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
				return
			}
			require.Error(t, err)
			require.Nil(t, got)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			if tc.wantErrText != "" {
				require.Contains(t, err.Error(), tc.wantErrText)
			}
		})
	}
}

func TestValidateVolumeClaimTemplates(t *testing.T) {
	tests := []struct {
		name    string
		vcts    []sandboxv1beta1.PersistentVolumeClaimTemplate
		wantErr error
	}{
		{name: "nil list is valid", vcts: nil},
		{name: "unique names are valid", vcts: vcts(vct("a", "x"), vct("b", "x"))},
		{name: "empty name is invalid", vcts: vcts(vct("a", "x"), vct("", "x")), wantErr: ErrVolumeClaimTemplatesInvalid},
		{name: "duplicate name is invalid", vcts: vcts(vct("a", "x"), vct("a", "y")), wantErr: ErrVolumeClaimTemplatesInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVolumeClaimTemplates(tc.vcts)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}
