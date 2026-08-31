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
	appsv1 "k8s.io/api/apps/v1"
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

type controllerWorkerConfig struct {
	SandboxWorkers int
	ClaimWorkers   int
	PoolWorkers    int
	MaxBatchSize   int
}

func (c controllerWorkerConfig) String() string {
	return fmt.Sprintf("sandbox:%d,claim:%d,pool:%d,batch:%d",
		c.SandboxWorkers, c.ClaimWorkers, c.PoolWorkers, c.MaxBatchSize)
}

// parseControllerWorkers parses SANDBOX_CONTROLLER_WORKERS.
// Format: "sandbox:20,claim:10,pool:1,batch:10"
func parseControllerWorkers() *controllerWorkerConfig {
	v := os.Getenv("SANDBOX_CONTROLLER_WORKERS")
	if v == "" {
		return nil
	}
	cfg := &controllerWorkerConfig{}
	for part := range strings.SplitSeq(v, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || n <= 0 {
			continue
		}
		switch strings.TrimSpace(key) {
		case "sandbox":
			cfg.SandboxWorkers = n
		case "claim":
			cfg.ClaimWorkers = n
		case "pool":
			cfg.PoolWorkers = n
		case "batch":
			cfg.MaxBatchSize = n
		}
	}
	return cfg
}

// applyControllerWorkerTuning patches the agent-sandbox-controller deployment
// with the worker counts from SANDBOX_CONTROLLER_WORKERS. Returns the parsed
// config (nil if unset) and a restore function that reverts to the original args.
func applyControllerWorkerTuning(t *testing.T, ctx context.Context, cl *framework.ClusterClient) (*controllerWorkerConfig, func()) {
	cfg := parseControllerWorkers()
	if cfg == nil {
		return nil, func() {}
	}

	key := types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}
	var deploy appsv1.Deployment
	require.NoError(t, cl.Get(ctx, key, &deploy))

	originalArgs := make([]string, len(deploy.Spec.Template.Spec.Containers[0].Args))
	copy(originalArgs, deploy.Spec.Template.Spec.Containers[0].Args)

	overrides := map[string]int{}
	if cfg.SandboxWorkers > 0 {
		overrides["--sandbox-concurrent-workers"] = cfg.SandboxWorkers
	}
	if cfg.ClaimWorkers > 0 {
		overrides["--sandbox-claim-concurrent-workers"] = cfg.ClaimWorkers
	}
	if cfg.PoolWorkers > 0 {
		overrides["--sandbox-warm-pool-concurrent-workers"] = cfg.PoolWorkers
	}
	if cfg.MaxBatchSize > 0 {
		overrides["--sandbox-warm-pool-max-batch-size"] = cfg.MaxBatchSize
	}

	newArgs := patchDeployArgs(deploy.Spec.Template.Spec.Containers[0].Args, overrides)
	t.Logf("[tuning] patching controller args: %v", newArgs)

	deploy.Spec.Template.Spec.Containers[0].Args = newArgs
	require.NoError(t, cl.Update(ctx, &deploy))
	waitForControllerRollout(t, ctx, cl, key)

	restore := func() {
		var current appsv1.Deployment
		if err := cl.Get(ctx, key, &current); err != nil {
			t.Logf("[tuning] restore: failed to get deployment: %v", err)
			return
		}
		current.Spec.Template.Spec.Containers[0].Args = originalArgs
		if err := cl.Update(ctx, &current); err != nil {
			t.Logf("[tuning] restore: failed to update deployment: %v", err)
			return
		}
		waitForControllerRollout(t, ctx, cl, key)
		t.Logf("[tuning] controller args restored")
	}
	return cfg, restore
}

func patchDeployArgs(args []string, overrides map[string]int) []string {
	result := make([]string, 0, len(args)+len(overrides))
	seen := make(map[string]bool)
	for _, arg := range args {
		flagName, _, _ := strings.Cut(arg, "=")
		if v, ok := overrides[flagName]; ok {
			result = append(result, fmt.Sprintf("%s=%d", flagName, v))
			seen[flagName] = true
		} else {
			result = append(result, arg)
		}
	}
	for flag, v := range overrides {
		if !seen[flag] {
			result = append(result, fmt.Sprintf("%s=%d", flag, v))
		}
	}
	return result
}

// waitForControllerRollout blocks until the controller deployment has rolled out
// all updated replicas and reports ready, or fails after 2 minutes.
func waitForControllerRollout(t *testing.T, ctx context.Context, cl *framework.ClusterClient, key types.NamespacedName) {
	t.Logf("[tuning] waiting for controller rollout...")
	timeout := 2 * time.Minute
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var deploy appsv1.Deployment
		require.NoError(t, cl.Get(deadline, key, &deploy))
		if deploy.Status.UpdatedReplicas == *deploy.Spec.Replicas &&
			deploy.Status.ReadyReplicas == *deploy.Spec.Replicas &&
			deploy.Status.ObservedGeneration >= deploy.Generation {
			t.Logf("[tuning] controller rollout complete (ready=%d)", deploy.Status.ReadyReplicas)
			return
		}
		select {
		case <-deadline.Done():
			t.Fatalf("[tuning] controller rollout timed out after %s", timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

func controllerWorkerCSVHeaders(cfg *controllerWorkerConfig) [][]string {
	if cfg == nil {
		return [][]string{{"# controller_workers", "default"}}
	}
	var headers [][]string
	headers = append(headers, []string{"# controller_workers", cfg.String()})
	if cfg.SandboxWorkers > 0 {
		headers = append(headers, []string{"# sandbox_concurrent_workers", strconv.Itoa(cfg.SandboxWorkers)})
	}
	if cfg.ClaimWorkers > 0 {
		headers = append(headers, []string{"# claim_concurrent_workers", strconv.Itoa(cfg.ClaimWorkers)})
	}
	if cfg.PoolWorkers > 0 {
		headers = append(headers, []string{"# pool_concurrent_workers", strconv.Itoa(cfg.PoolWorkers)})
	}
	if cfg.MaxBatchSize > 0 {
		headers = append(headers, []string{"# max_batch_size", strconv.Itoa(cfg.MaxBatchSize)})
	}
	return headers
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
