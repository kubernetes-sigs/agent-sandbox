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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestMatchesGroupKind(t *testing.T) {
	testCases := []struct {
		name      string
		ref       *metav1.OwnerReference
		group     string
		kind      string
		wantMatch bool
	}{
		{
			name:      "nil reference",
			ref:       nil,
			group:     extensionsv1beta1.GroupVersion.Group,
			kind:      extensionsv1beta1.SandboxWarmPoolKind,
			wantMatch: false,
		},
		{
			name:      "nil reference with empty group and kind",
			ref:       nil,
			group:     "",
			kind:      "",
			wantMatch: false,
		},
		{
			name: "exact match",
			ref: &metav1.OwnerReference{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
			},
			group:     extensionsv1beta1.GroupVersion.Group,
			kind:      extensionsv1beta1.SandboxWarmPoolKind,
			wantMatch: true,
		},
		{
			name: "group mismatch",
			ref: &metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
			},
			group:     extensionsv1beta1.GroupVersion.Group,
			kind:      extensionsv1beta1.SandboxWarmPoolKind,
			wantMatch: false,
		},
		{
			name: "kind mismatch",
			ref: &metav1.OwnerReference{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxClaimKind,
			},
			group:     extensionsv1beta1.GroupVersion.Group,
			kind:      extensionsv1beta1.SandboxWarmPoolKind,
			wantMatch: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchesGroupKind(tc.ref, tc.group, tc.kind)
			require.Equal(t, tc.wantMatch, got)
		})
	}
}

func TestGetGroupKind(t *testing.T) {
	trueVal := true
	testCases := []struct {
		name      string
		ref       *metav1.OwnerReference
		wantGroup string
		wantKind  string
	}{
		{
			name:      "nil reference",
			ref:       nil,
			wantGroup: "",
			wantKind:  "",
		},
		{
			name: "valid reference",
			ref: &metav1.OwnerReference{
				APIVersion: extensionsv1beta1.GroupVersion.String(),
				Kind:       extensionsv1beta1.SandboxWarmPoolKind,
				Name:       "my-warmpool",
				UID:        "5678",
				Controller: &trueVal,
			},
			wantGroup: extensionsv1beta1.GroupVersion.Group,
			wantKind:  extensionsv1beta1.SandboxWarmPoolKind,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g, k := GetGroupKind(tc.ref)
			require.Equal(t, tc.wantGroup, g)
			require.Equal(t, tc.wantKind, k)
		})
	}
}
