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
	"fmt"
	"os"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestRuntimeClassLifecycle validates the full SandboxTemplate → WarmPool →
// SandboxClaim → refill cycle with a caller-specified RuntimeClassName.
//
// Set SANDBOX_RUNTIME_CLASS to the desired RuntimeClass name (e.g. gvisor,
// kata-qemu, kata-clh). Use "default" for the cluster's default runtime
// (leaves RuntimeClassName unset). The test is skipped when the variable is
// unset, so existing CI is unaffected.
func TestRuntimeClassLifecycle(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping runtime class lifecycle test")
	}

	tc := framework.NewTestContext(t)

	cluster, err := tc.ClusterInfo(t.Context())
	require.NoError(t, err)

	replicas := int32(2)
	if isVMRuntime(runtimeClass) && cluster.TotalCPUCapacity < int64(replicas) {
		replicas = int32(cluster.TotalCPUCapacity)
	}
	if replicas < 1 {
		t.Skip("not enough CPU capacity for warm pool replicas")
	}
	t.Logf("[config] runtimeClass=%s replicas=%d k8s=%s provider=%s cpus=%d",
		runtimeClass, replicas, cluster.KubernetesVersion, cluster.Provider, cluster.TotalCPUCapacity)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("runtime-class-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// SandboxTemplate with the requested RuntimeClassName.
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-template",
			Namespace: ns.Name,
		},
	}
	rcPtr := runtimeClassPtrFromEnv(runtimeClass)
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			RuntimeClassName: rcPtr,
			Containers: []corev1.Container{
				{
					Name:            "pause",
					Image:           "registry.k8s.io/pause:3.10",
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-warmpool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
	t.Logf("Waiting for WarmPool to reach %d ready replicas (runtimeClass=%s)...", replicas, runtimeClass)
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	// Verify pool sandboxes carry the RuntimeClassName.
	sandboxList := &sandboxv1beta1.SandboxList{}
	require.NoError(t, tc.List(t.Context(), sandboxList, client.InNamespace(ns.Name)))
	var poolSandboxes []sandboxv1beta1.Sandbox
	for i := range sandboxList.Items {
		sb := &sandboxList.Items[i]
		if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(sb, warmPool) {
			poolSandboxes = append(poolSandboxes, *sb)
		}
	}
	require.Len(t, poolSandboxes, int(replicas), "expected %d pool sandboxes", replicas)

	for i := range poolSandboxes {
		sb := &poolSandboxes[i]
		require.Equal(t, rcPtr, sb.Spec.PodTemplate.Spec.RuntimeClassName,
			"Sandbox %s RuntimeClassName should match requested value", sb.Name)

		pod := &corev1.Pod{}
		pod.Name = sb.Name
		pod.Namespace = ns.Name
		tc.MustWaitForObject(pod, predicates.ReadyConditionIsTrue)
		require.Equal(t, rcPtr, pod.Spec.RuntimeClassName,
			"Pod %s RuntimeClassName should match requested value", pod.Name)
	}

	// --- Claim 1: consume a sandbox, verify pool refills ---
	claim1 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-claim-1",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle:   claimLifecycle,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim1))
	t.Logf("Waiting for claim-1 to be ready...")
	tc.MustWaitForObject(claim1, predicates.ReadyConditionIsTrue)

	t.Logf("Waiting for pool to observe consumed sandbox...")
	require.Eventually(t, func() bool {
		pool := &extensionsv1beta1.SandboxWarmPool{}
		if err := tc.Get(t.Context(), warmPoolID, pool); err != nil {
			return false
		}
		return pool.Status.ReadyReplicas < replicas
	}, framework.DefaultTimeout, time.Second, "pool should observe consumed sandbox")

	t.Logf("Waiting for pool to refill to %d replicas...", replicas)
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	// --- Claim 2: verify the refilled pool serves another claim ---
	claim2 := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-claim-2",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name},
			Lifecycle:   claimLifecycle,
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim2))
	t.Logf("Waiting for claim-2 to be ready...")
	tc.MustWaitForObject(claim2, predicates.ReadyConditionIsTrue)

	t.Logf("RuntimeClass %q lifecycle test passed: pool fill → claim → refill → claim", runtimeClass)
}

// TestRuntimeClassStartupComparison measures the difference between creating a
// sandbox from scratch (cold start) and claiming one from a pre-warmed pool.
// Both use the RuntimeClassName from the SANDBOX_RUNTIME_CLASS env var.
//
// Run with:
//
//	SANDBOX_RUNTIME_CLASS=gvisor go test ./test/e2e/extensions/... -run TestRuntimeClassStartupComparison -v -timeout 5m
func TestRuntimeClassStartupComparison(t *testing.T) {
	runtimeClass := os.Getenv("SANDBOX_RUNTIME_CLASS")
	if runtimeClass == "" {
		t.Skip("SANDBOX_RUNTIME_CLASS not set — skipping startup comparison test")
	}

	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("runtime-bench-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	podSpec := corev1.PodSpec{
		RuntimeClassName: runtimeClassPtrFromEnv(runtimeClass),
		Containers: []corev1.Container{
			{
				Name:            "pause",
				Image:           "registry.k8s.io/pause:3.10",
				ImagePullPolicy: corev1.PullIfNotPresent,
			},
		},
	}

	coldDuration := baselineColdStart(t, tc, ns.Name, podSpec)

	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-template",
			Namespace: ns.Name,
		},
	}
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{Spec: podSpec}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	replicas := int32(1)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-warmpool",
			Namespace: ns.Name,
		},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))

	warmPoolID := types.NamespacedName{Name: warmPool.Name, Namespace: ns.Name}
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), warmPoolID))

	claimDuration, _ := baselineWarmClaim(t, tc, ns.Name, warmPool.Name)

	t.Logf("=== Startup Comparison (runtimeClass=%s) ===", runtimeClass)
	t.Logf("  Cold start:  %s", coldDuration)
	t.Logf("  Warm claim:  %s", claimDuration)
	if claimDuration > 0 {
		speedup := float64(coldDuration) / float64(claimDuration)
		t.Logf("  Speedup:     %.1fx", speedup)
	}
}
