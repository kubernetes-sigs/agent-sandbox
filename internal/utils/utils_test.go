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
	"k8s.io/apimachinery/pkg/types"
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

func TestRefreshedOwnerReferences(t *testing.T) {
	const owner = types.UID("owner-uid")
	const beta = "agents.x-k8s.io/v1beta1"
	testCases := []struct {
		name        string
		refs        []metav1.OwnerReference
		wantChanged bool
		wantVersion []string
	}{
		{
			name:        "v1alpha1 reference to the owner is refreshed",
			refs:        []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1alpha1", Kind: "Sandbox", Name: "sb", UID: owner}},
			wantChanged: true,
			wantVersion: []string{beta},
		},
		{
			name:        "current reference is left alone",
			refs:        []metav1.OwnerReference{{APIVersion: beta, Kind: "Sandbox", Name: "sb", UID: owner}},
			wantChanged: false,
		},
		{
			name: "references to other owners are left alone",
			refs: []metav1.OwnerReference{
				{APIVersion: "agents.x-k8s.io/v1alpha1", Kind: "Sandbox", Name: "other", UID: "other-uid"},
				{APIVersion: "agents.x-k8s.io/v1alpha1", Kind: "Sandbox", Name: "sb", UID: owner},
			},
			wantChanged: true,
			wantVersion: []string{"agents.x-k8s.io/v1alpha1", beta},
		},
		{
			name:        "a reference with the owner's UID in another group is left alone",
			refs:        []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: owner}},
			wantChanged: false,
		},
		{
			name:        "no owner references",
			wantChanged: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]metav1.OwnerReference(nil), tc.refs...)
			refreshed, changed := RefreshedOwnerReferences(tc.refs, owner, beta)
			require.Equal(t, tc.wantChanged, changed)
			require.Equal(t, original, tc.refs, "the input must not be modified")
			if !tc.wantChanged {
				require.Nil(t, refreshed)
				return
			}
			var got []string
			for _, ref := range refreshed {
				got = append(got, ref.APIVersion)
			}
			require.Equal(t, tc.wantVersion, got)
		})
	}
}
