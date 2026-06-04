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

package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/examples/managed-sandbox/api/v1alpha1"
)

// DefaultGCInterval is the cadence at which the controller asks each pool
// pod's agent to list its tenants and reaps any that no longer correspond
// to a live, non-deleting Sandbox CR.
const DefaultGCInterval = 30 * time.Minute

// GC reaps orphaned bubblewrap tenants. We deliberately avoid finalizers
// on the Sandbox CR (they're operationally painful when the controller or
// pod-agent is degraded), so a Sandbox can be deleted at any time and the
// runtime tenant lives on until the next sweep cleans it up.
//
// One sweep visits every pool pod, lists its tenants, and asks the agent
// to delete any tenant whose sandbox_uid does not correspond to a live
// Sandbox CR (or whose CR is being deleted).
type GC struct {
	Client client.Client
	Agents *AgentClientPool
}

// RunForever starts the GC: one sweep immediately, then one every interval
// until ctx is cancelled. Errors from individual sweeps are logged but do
// not stop the loop.
func (g *GC) RunForever(ctx context.Context, interval time.Duration) {
	log := log.FromContext(ctx).WithName("pool-gc")
	if interval <= 0 {
		interval = DefaultGCInterval
	}
	if err := g.SweepOnce(ctx); err != nil {
		log.Error(err, "initial GC sweep failed")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.SweepOnce(ctx); err != nil {
				log.Error(err, "GC sweep failed")
			}
		}
	}
}

// SweepOnce executes one GC pass across every pool pod in every namespace.
// It returns the first error encountered (other errors are logged) so the
// caller can surface failures in tests; in steady state RunForever ignores
// the return value.
func (g *GC) SweepOnce(ctx context.Context) error {
	log := log.FromContext(ctx).WithName("pool-gc")
	pods := &corev1.PodList{}
	if err := g.Client.List(ctx, pods,
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(map[string]string{
			LabelManagedBy: LabelManagedByValue,
		})},
	); err != nil {
		return fmt.Errorf("gc: list pool pods: %w", err)
	}

	// Build set of live sandbox UIDs (across all namespaces) — Sandboxes
	// being deleted are intentionally excluded so they reap promptly.
	live, err := g.liveSandboxUIDs(ctx)
	if err != nil {
		return fmt.Errorf("gc: list sandboxes: %w", err)
	}

	var firstErr error
	for i := range pods.Items {
		p := &pods.Items[i]
		if !IsPodReady(p) || p.Status.PodIP == "" {
			continue
		}
		if err := g.sweepPod(ctx, p, live); err != nil {
			log.Error(err, "gc: sweep pod failed", "Pool.Pod", p.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (g *GC) sweepPod(ctx context.Context, pod *corev1.Pod, live map[string]struct{}) error {
	log := log.FromContext(ctx).WithName("pool-gc").WithValues("Pool.Pod", pod.Name)
	agent, err := g.Agents.For(ctx, pod.Name, pod.Status.PodIP)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	tenants, err := agent.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for _, t := range tenants {
		if _, ok := live[t.UID]; ok {
			continue
		}
		log.Info("Reaping orphan tenant", "Sandbox.UID", t.UID, "Tenant.Phase", t.Phase)
		if err := agent.DeleteSandbox(ctx, t.UID); err != nil && !errors.Is(err, ErrNotFound) {
			log.Error(err, "delete tenant failed", "Sandbox.UID", t.UID)
		}
	}
	return nil
}

func (g *GC) liveSandboxUIDs(ctx context.Context) (map[string]struct{}, error) {
	list := &sandboxv1alpha1.ManagedSandboxList{}
	if err := g.Client.List(ctx, list); err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		out[string(s.UID)] = struct{}{}
	}
	return out, nil
}
