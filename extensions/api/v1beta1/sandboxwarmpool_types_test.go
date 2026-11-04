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

package v1beta1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWarmPoolCapacityDistinguishesZeroFromUnknown(t *testing.T) {
	pool := &SandboxWarmPool{Spec: SandboxWarmPoolSpec{ReplenishmentCapacity: new(int32(0))}, Status: SandboxWarmPoolStatus{OccupiedReplicas: new(int32(0)), DesiredStandbyReplicas: new(int32(0))}}
	data, err := json.Marshal(pool)
	require.NoError(t, err)
	require.Contains(t, string(data), `"replenishmentCapacity":0`)
	require.Contains(t, string(data), `"occupiedReplicas":0`)
	require.Contains(t, string(data), `"desiredStandbyReplicas":0`)
	cloned := pool.DeepCopy()
	*cloned.Spec.ReplenishmentCapacity = 7
	*cloned.Status.OccupiedReplicas = 7
	*cloned.Status.DesiredStandbyReplicas = 7
	require.Zero(t, *pool.Spec.ReplenishmentCapacity)
	require.Zero(t, *pool.Status.OccupiedReplicas)
	require.Zero(t, *pool.Status.DesiredStandbyReplicas)
	data, err = json.Marshal(&SandboxWarmPool{})
	require.NoError(t, err)
	require.NotContains(t, string(data), "replenishmentCapacity")
	require.NotContains(t, string(data), "occupiedReplicas")
	require.NotContains(t, string(data), "desiredStandbyReplicas")
}
