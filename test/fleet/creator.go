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

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// runCreates drives the aggregate paced create load. One global rate
// limiter enforces the fleet-wide rate; per-cluster worker pools do the
// creating. Each sandbox's ordinal hashes to its cluster, so a given ID
// always lands on the same cluster (restartable, and analysis can
// recompute the assignment offline).
func runCreates(ctx context.Context, cfg config, clusters []*cluster, rec *recorder, start time.Time) error {
	n := len(clusters)

	// Assign every ordinal to its hashed cluster in one up-front pass,
	// into per-cluster id lists. This keeps the (deliberately non-uniform)
	// fnv distribution off the per-create hot path AND removes the single
	// pacer goroutine that used to hand ids to per-cluster channels: that
	// pacer blocked on a full channel whenever ONE cluster fell behind,
	// starving every other cluster behind the slowest one (the ~10k/s
	// fleet ceiling where all clusters throttled equally). Each cluster
	// now drives its own share independently; a slow cluster only slows
	// itself.
	ids := make([][]int, n)
	for id := 0; id < cfg.Count; id++ {
		ci := clusterFor(id, n)
		ids[ci] = append(ids[ci], id)
	}

	// Per-cluster rate limiters, each at Rate/N. A single fleet-wide shared
	// limiter looks fair but is NOT under WAN latency: low-latency (near)
	// clusters' workers complete a create and come back for the next token
	// far sooner than high-latency (far) clusters' workers, so the near
	// clusters win a disproportionate share of tokens and drain first while
	// the far clusters are starved below their share. That staggers the
	// per-cluster readiness peaks across time (near clusters peak early and
	// finish, far clusters peak late), and the fleet's single 60s verdict
	// window can't capture peaks that don't overlap - measured 104k (~10%)
	// lost to staggering at 20 clusters. A dedicated limiter per cluster
	// paces every cluster to exactly Rate/N regardless of distance, so the
	// peaks align and the fleet window captures them together.
	burst := cfg.CreatePerCluster
	if burst < 1 {
		burst = 1
	}
	perClusterRate := rate.Limit(cfg.Rate / float64(n))
	limiters := make([]*rate.Limiter, n)
	for i := range limiters {
		limiters[i] = rate.NewLimiter(perClusterRate, burst)
	}

	// Progress logger.
	logDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-logDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				var created, ready int64
				for _, c := range clusters {
					created += c.created.Load()
					ready += c.ready.Load()
				}
				log.Printf("[fleet +%s] created=%d ready=%d", time.Since(start).Round(time.Second), created, ready)
			}
		}
	}()

	var wg sync.WaitGroup
	for ci, c := range clusters {
		myIDs := ids[ci]
		for w := 0; w < cfg.CreatePerCluster; w++ {
			wg.Add(1)
			// Pin each worker to one of the cluster's sharded clients
			// (round-robin over connections) and to a disjoint stride of
			// this cluster's ids - no per-create client selection, no
			// coordination between workers.
			res := c.clients[w%len(c.clients)].Resource(gvrSandboxes).Namespace(cfg.Namespace)
			limiter := limiters[ci]
			go func(c *cluster, res dynamicCreator, limiter *rate.Limiter, w int) {
				defer wg.Done()
				for k := w; k < len(myIDs); k += cfg.CreatePerCluster {
					id := myIDs[k]
					if err := limiter.Wait(ctx); err != nil {
						return
					}
					obj := buildSandbox(cfg, id)
					t0 := time.Now()
					_, err := res.Create(ctx, obj, metav1.CreateOptions{})
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						rec.RecordCreateError(c, id, err)
						continue
					}
					c.created.Add(1)
					rec.RecordCreate(c, id, t0, time.Since(t0))
				}
			}(c, res, limiter, w)
		}
	}
	wg.Wait()
	close(logDone)

	var created int64
	for _, c := range clusters {
		created += c.created.Load()
	}
	log.Printf("[fleet] creates done: %d/%d in %s", created, cfg.Count, time.Since(start).Round(time.Second))
	return ctx.Err()
}

// dynamicCreator is the create-only slice of the dynamic resource client
// each worker holds (pinned to one connection shard).
type dynamicCreator interface {
	Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error)
}

func buildSandbox(cfg config, id int) *unstructured.Unstructured {
	name := fmt.Sprintf("fl-%d", id)
	spec := map[string]any{
		"podTemplate": map[string]any{
			"spec": map[string]any{
				"restartPolicy": "Never",
				// sleep(1) never handles SIGTERM, so every second of grace
				// is pure teardown latency: 100k pods x 30s default grace
				// blew a fleet cleanup past its timeout.
				"terminationGracePeriodSeconds": 0,
				// Critical for launch throughput: without this the kubelet
				// projects a service-account token volume per pod, which
				// means a TokenRequest call to the apiserver at every pod
				// start - 100k extra control-plane round-trips on the exact
				// path we are trying to keep fast. Sandboxes don't need it.
				"automountServiceAccountToken": false,
				// Keep sandbox pods off dedicated nodes (the pinned
				// controller's node): kOps-on-GCE cannot render IG taints,
				// so exclusion has to come from the workload side.
				"affinity": map[string]any{
					"nodeAffinity": map[string]any{
						"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
							"nodeSelectorTerms": []any{
								map[string]any{
									"matchExpressions": []any{
										map[string]any{
											"key":      "dedicated",
											"operator": "DoesNotExist",
										},
									},
								},
							},
						},
					},
				},
				"containers": []any{
					map[string]any{
						"name":  "main",
						"image": cfg.Image,
						// Explicit pull policy: ':latest' tags default to
						// Always, which makes EVERY pod start a registry
						// round-trip despite the prepull - Docker Hub
						// backoffs turned a smoke launch into 15s waves.
						"imagePullPolicy": "IfNotPresent",
						"command":         []any{"sleep", "infinity"},
						// Non-zero request so scheduler spreading and node
						// capacity math behave like real workloads (and, on
						// lane-partitioned clusters, so nodes cannot be
						// stuffed past their partition share).
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "5m"},
						},
					},
				},
			},
		},
	}
	if len(cfg.SchedulerNames) > 0 {
		lane := laneFor(name, len(cfg.SchedulerNames))
		podSpec := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)
		if cfg.SchedulerNames[lane] != "default-scheduler" {
			podSpec["schedulerName"] = cfg.SchedulerNames[lane]
		}
		if cfg.PartitionLabel != "" {
			podSpec["nodeSelector"] = map[string]any{cfg.PartitionLabel: fmt.Sprintf("%d", lane)}
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1",
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// laneFor mirrors the single-cluster harness's lane hashing so lane
// balance holds per cluster.
func laneFor(name string, lanes int) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % uint32(lanes))
}
