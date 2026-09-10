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

package v1beta1

import "testing"

func TestPersistentVolumeClaimRetentionPolicyEffectiveWhenDeleted(t *testing.T) {
	var nilPolicy *PersistentVolumeClaimRetentionPolicy
	emptyPolicy := &PersistentVolumeClaimRetentionPolicy{}
	tests := []struct {
		name   string
		policy *PersistentVolumeClaimRetentionPolicy
		want   PersistentVolumeClaimRetentionPolicyType
	}{
		{name: "nil", policy: nilPolicy, want: PersistentVolumeClaimRetentionPolicyDelete},
		{name: "empty", policy: emptyPolicy, want: PersistentVolumeClaimRetentionPolicyDelete},
		{name: "Delete", policy: &PersistentVolumeClaimRetentionPolicy{WhenDeleted: PersistentVolumeClaimRetentionPolicyDelete}, want: PersistentVolumeClaimRetentionPolicyDelete},
		{name: "Retain", policy: &PersistentVolumeClaimRetentionPolicy{WhenDeleted: PersistentVolumeClaimRetentionPolicyRetain}, want: PersistentVolumeClaimRetentionPolicyRetain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.EffectiveWhenDeleted(); got != tt.want {
				t.Fatalf("EffectiveWhenDeleted() = %q, want %q", got, tt.want)
			}
		})
	}
}
