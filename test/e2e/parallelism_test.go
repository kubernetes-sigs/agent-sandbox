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

package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

func patchControllerConcurrency(t *testing.T, tc *framework.TestContext, workers int) func() {
	var originalDeployment appsv1.Deployment
	err := tc.Get(t.Context(), types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, &originalDeployment)
	require.NoError(t, err, "failed to get controller deployment")

	deployment := originalDeployment.DeepCopy()
	// Update container args
	for i, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name == "agent-sandbox-controller" {
			newArgs := []string{}
			// Keep existing non-concurrency args
			for _, arg := range c.Args {
				if arg != fmt.Sprintf("--sandbox-concurrent-workers=%d", workers) &&
					arg != fmt.Sprintf("--sandbox-claim-concurrent-workers=%d", workers) &&
					arg != fmt.Sprintf("--sandbox-warm-pool-concurrent-workers=%d", workers) &&
					arg != "--kube-api-qps=50" &&
					arg != "--kube-api-burst=100" {
					newArgs = append(newArgs, arg)
				}
			}
			newArgs = append(newArgs, fmt.Sprintf("--sandbox-concurrent-workers=%d", workers))
			newArgs = append(newArgs, fmt.Sprintf("--sandbox-claim-concurrent-workers=%d", workers))
			newArgs = append(newArgs, fmt.Sprintf("--sandbox-warm-pool-concurrent-workers=%d", workers))
			newArgs = append(newArgs, "--kube-api-qps=50")
			newArgs = append(newArgs, "--kube-api-burst=100")
			deployment.Spec.Template.Spec.Containers[i].Args = newArgs
			break
		}
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest appsv1.Deployment
		if err := tc.Get(t.Context(), types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, &latest); err != nil {
			return err
		}
		latest.Spec = deployment.Spec
		return tc.Update(t.Context(), &latest)
	})
	require.NoError(t, err, "failed to update controller deployment")

	// Wait for the new pod to be ready
	err = tc.WaitForObject(t.Context(), deployment, []predicates.ObjectPredicate{
		predicates.ReadyReplicasConditionIsTrue,
	}...)

	require.NoError(t, err, "failed to wait for controller deployment")
	time.Sleep(5 * time.Second) // Give the leader election time to settle

	return func() {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest appsv1.Deployment
			if err := tc.Get(t.Context(), types.NamespacedName{Name: "agent-sandbox-controller", Namespace: "agent-sandbox-system"}, &latest); err != nil {
				return err
			}
			latest.Spec = originalDeployment.Spec
			return tc.Update(t.Context(), &latest)
		})
		require.NoError(t, err, "failed to restore controller deployment")
	}
}

func TestParallelSandboxes(t *testing.T) {
	tc := framework.NewTestContext(t)
	cleanup := patchControllerConcurrency(t, tc, 10)
	defer cleanup()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("parallel-sandboxes-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	numSandboxes := 20
	var wg sync.WaitGroup
	errCh := make(chan error, numSandboxes)

	for i := 0; i < numSandboxes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sandboxName := fmt.Sprintf("sandbox-%d", idx)
			sandboxObj := simpleSandbox(ns.Name)
			sandboxObj.Name = sandboxName
			if err := tc.CreateWithCleanup(t.Context(), sandboxObj); err != nil {
				errCh <- fmt.Errorf("failed creating sandbox %d: %w", idx, err)
				return
			}
			if err := tc.WaitForObject(t.Context(), sandboxObj, predicates.ReadyConditionIsTrue); err != nil {
				errCh <- fmt.Errorf("failed waiting for sandbox %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Error during parallel run: %v", err)
	}
}

func TestParallelSandboxClaims(t *testing.T) {
	tc := framework.NewTestContext(t)
	cleanup := patchControllerConcurrency(t, tc, 10)
	defer cleanup()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("parallel-claims-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "test-template-claims"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "pause", Image: "registry.k8s.io/pause:3.10"},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	numClaims := 20
	var wg sync.WaitGroup
	errCh := make(chan error, numClaims)

	for i := 0; i < numClaims; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			claimName := fmt.Sprintf("claim-%d", idx)
			claimObj := &extensionsv1alpha1.SandboxClaim{}
			claimObj.Name = claimName
			claimObj.Namespace = ns.Name
			claimObj.Spec.TemplateRef.Name = template.Name
			if err := tc.CreateWithCleanup(t.Context(), claimObj); err != nil {
				errCh <- fmt.Errorf("failed creating claim %d: %w", idx, err)
				return
			}
			if err := tc.WaitForObject(t.Context(), claimObj, predicates.ReadyConditionIsTrue); err != nil {
				errCh <- fmt.Errorf("failed waiting for claim %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Error during parallel run: %v", err)
	}
}

func TestParallelSandboxClaimsWithSufficientWarmPool(t *testing.T) {
	tc := framework.NewTestContext(t)
	cleanup := patchControllerConcurrency(t, tc, 10)
	defer cleanup()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("parallel-claims-suff-pool-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "test-template-suff"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "pause", Image: "registry.k8s.io/pause:3.10"},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	poolObj := &extensionsv1alpha1.SandboxWarmPool{}
	poolObj.Name = "warmpool-sufficient"
	poolObj.Namespace = ns.Name
	// Pool size is explicitly set to handle all claims plus some buffer
	poolSize := int32(25)
	poolObj.Spec.Replicas = poolSize
	poolObj.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), poolObj))

	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), types.NamespacedName{Name: poolObj.Name, Namespace: poolObj.Namespace}))

	numClaims := 20
	var wg sync.WaitGroup
	errCh := make(chan error, numClaims)

	for i := 0; i < numClaims; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			claimName := fmt.Sprintf("claim-%d", idx)
			claimObj := &extensionsv1alpha1.SandboxClaim{}
			claimObj.Name = claimName
			claimObj.Namespace = ns.Name
			claimObj.Spec.TemplateRef.Name = template.Name
			if err := tc.CreateWithCleanup(t.Context(), claimObj); err != nil {
				errCh <- fmt.Errorf("failed creating claim %d: %w", idx, err)
				return
			}
			if err := tc.WaitForObject(t.Context(), claimObj, predicates.ReadyConditionIsTrue); err != nil {
				errCh <- fmt.Errorf("failed waiting for claim %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Error during parallel run: %v", err)
	}
}

func TestParallelSandboxClaimsWithInsufficientWarmPool(t *testing.T) {
	tc := framework.NewTestContext(t)
	cleanup := patchControllerConcurrency(t, tc, 10)
	defer cleanup()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("parallel-claims-insuff-pool-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	// Create a SandboxTemplate
	template := &extensionsv1alpha1.SandboxTemplate{}
	template.Name = "test-template-insuff"
	template.Namespace = ns.Name
	template.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "pause", Image: "registry.k8s.io/pause:3.10"},
			},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	poolObj := &extensionsv1alpha1.SandboxWarmPool{}
	poolObj.Name = "warmpool-insufficient"
	poolObj.Namespace = ns.Name
	// Pool size is explicitly set to handle less claims than total
	poolSize := int32(5)
	poolObj.Spec.Replicas = poolSize
	poolObj.Spec.TemplateRef.Name = template.Name
	require.NoError(t, tc.CreateWithCleanup(t.Context(), poolObj))

	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), types.NamespacedName{Name: poolObj.Name, Namespace: poolObj.Namespace}))

	numClaims := 20
	var wg sync.WaitGroup
	errCh := make(chan error, numClaims)

	for i := 0; i < numClaims; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			claimName := fmt.Sprintf("claim-%d", idx)
			claimObj := &extensionsv1alpha1.SandboxClaim{}
			claimObj.Name = claimName
			claimObj.Namespace = ns.Name
			claimObj.Spec.TemplateRef.Name = template.Name
			if err := tc.CreateWithCleanup(t.Context(), claimObj); err != nil {
				errCh <- fmt.Errorf("failed creating claim %d: %w", idx, err)
				return
			}
			if err := tc.WaitForObject(t.Context(), claimObj, predicates.ReadyConditionIsTrue); err != nil {
				errCh <- fmt.Errorf("failed waiting for claim %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Error during parallel run: %v", err)
	}
}
