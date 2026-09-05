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

package extensions

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func isVMRuntime(runtimeClass string) bool {
	return strings.HasPrefix(runtimeClass, "kata")
}

func runtimeClassPtrFromEnv(value string) *string {
	if value == "default" {
		return nil
	}
	return &value
}

var claimTTL = func() int32 {
	if v := os.Getenv("SANDBOX_TTL"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return int32(n)
		}
	}
	return 0
}()

var claimLifecycle = &extensionsv1beta1.Lifecycle{
	ShutdownPolicy:          extensionsv1beta1.ShutdownPolicyDelete,
	TTLSecondsAfterFinished: &claimTTL,
}

func baselineColdStart(t *testing.T, tc *framework.TestContext, ns string, podSpec corev1.PodSpec) time.Duration {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cold-baseline-%d", time.Now().UnixNano()),
			Namespace: ns,
		},
	}
	sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}

	t.Logf("[baseline] measuring cold start...")
	start := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sandbox))
	tc.MustWaitForObject(sandbox, predicates.ReadyConditionIsTrue)
	d := time.Since(start)

	require.NoError(t, tc.Delete(t.Context(), sandbox))
	t.Logf("[baseline] cold start: %.3fs", d.Seconds())
	return d
}

func baselineWarmClaim(t *testing.T, tc *framework.TestContext, ns, poolName string) (time.Duration, *extensionsv1beta1.SandboxClaim) {
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("warm-baseline-%d", time.Now().UnixNano()),
			Namespace: ns,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: poolName},
			Lifecycle:   claimLifecycle,
		},
	}

	t.Logf("[baseline] measuring warm claim...")
	start := time.Now()
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))
	tc.MustWaitForObject(claim, predicates.ReadyConditionIsTrue)
	d := time.Since(start)
	t.Logf("[baseline] warm claim: %.3fs", d.Seconds())
	return d, claim
}

func baselinePoolFill(t *testing.T, tc *framework.TestContext, pool *extensionsv1beta1.SandboxWarmPool, poolID types.NamespacedName, replicas int32, timeout time.Duration) time.Duration {
	framework.MustUpdateObject(tc.ClusterClient, pool, func(p *extensionsv1beta1.SandboxWarmPool) {
		p.Spec.Replicas = &replicas
	})

	t.Logf("[baseline] filling pool to %d replicas...", replicas)
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	require.NoError(t, tc.WaitForWarmPoolReady(ctx, poolID))
	d := time.Since(start)
	t.Logf("[baseline] pool-%d filled in %.3fs", replicas, d.Seconds())
	return d
}

func benchPoolSizes(cpuCapacity int64) ([]int, error) {
	if v := os.Getenv("SANDBOX_POOL_SIZES"); v != "" {
		var sizes []int
		for s := range strings.SplitSeq(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return nil, fmt.Errorf("invalid SANDBOX_POOL_SIZES value %q: %w", s, err)
			}
			if n <= 0 {
				return nil, fmt.Errorf("invalid SANDBOX_POOL_SIZES value %q: must be positive", s)
			}
			sizes = append(sizes, n)
		}
		return sizes, nil
	}
	if cpuCapacity > 0 {
		half := max(int(cpuCapacity/2), 1)
		full := int(cpuCapacity)
		double := full * 2
		sizes := slices.Compact([]int{half, full, double})
		return sizes, nil
	}
	return nil, fmt.Errorf("cluster reported 0 worker CPU capacity — cannot derive pool sizes")
}
