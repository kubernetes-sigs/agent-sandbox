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

package main

import "testing"

// The fleet verdict depends on every cluster carrying its share: at
// 16 clusters x ~62.5k sandboxes, a 5% imbalance is ~3k extra sandboxes
// on one cluster - enough to make it the straggler that decides the
// 60s window. Guard the hash distribution stays within 2% of even.
func TestClusterDistributionEven(t *testing.T) {
	for _, n := range []int{2, 13, 16} {
		counts := make([]int, n)
		total := 1000000
		for id := 0; id < total; id++ {
			counts[clusterFor(id, n)]++
		}
		want := total / n
		for i, got := range counts {
			skew := float64(got-want) / float64(want)
			if skew > 0.02 || skew < -0.02 {
				t.Errorf("n=%d cluster %d: %d sandboxes, %.1f%% from even", n, i, got, skew*100)
			}
		}
	}
}

func TestClusterForDeterministic(t *testing.T) {
	for id := 0; id < 1000; id++ {
		if clusterFor(id, 16) != clusterFor(id, 16) {
			t.Fatalf("clusterFor not deterministic at id %d", id)
		}
	}
}
