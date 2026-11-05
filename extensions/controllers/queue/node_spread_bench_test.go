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

package queue_test

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

const benchmarkNodeSpreadPool = "benchmark/warm-pool"

// Copied from sandboxclaim_controller.go at 4d39311ba9d89001d79ff1217ed330c7ea3761ec.
var benchmarkOriginalNodeSpread = func(keys []queue.SandboxKey) (queue.SandboxKey, bool) {
	namespaceKeys := keys

	if len(namespaceKeys) == 0 {
		return queue.SandboxKey{}, false
	}
	if len(namespaceKeys) == 1 {
		return namespaceKeys[0], true
	}

	// Group candidates into scheduled vs unscheduled
	var scheduledKeys []queue.SandboxKey
	var unscheduledKeys []queue.SandboxKey
	for _, key := range namespaceKeys {
		if key.NodeName != "" {
			scheduledKeys = append(scheduledKeys, key)
		} else {
			unscheduledKeys = append(unscheduledKeys, key)
		}
	}

	// NodeSpread strategy: spread workloads by round-robinning nodes.
	// We count the remaining warmpool sandboxes per node in the queue.
	// The node with the most remaining sandboxes has been selected the least.
	if len(scheduledKeys) > 0 {
		nodeCounts := make(map[string]int)
		for _, key := range scheduledKeys {
			nodeCounts[key.NodeName]++
		}

		maxCount := 0
		for _, count := range nodeCounts {
			if count > maxCount {
				maxCount = count
			}
		}

		var bestCandidates []queue.SandboxKey
		for _, key := range scheduledKeys {
			if nodeCounts[key.NodeName] == maxCount {
				bestCandidates = append(bestCandidates, key)
			}
		}

		// Ties (equal counts) are resolved using oldest first (first in the slice)
		return bestCandidates[0], true
	}

	// Fall back to oldest first (FIFO) for unscheduled keys
	return unscheduledKeys[0], true
}

func benchmarkNodeSpreadGetter(q *queue.SimpleSandboxQueue) func(string) (queue.SandboxKey, bool) {
	if indexed, ok := any(q).(interface {
		GetWithNodeSpread(string) (queue.SandboxKey, bool)
	}); ok {
		return indexed.GetWithNodeSpread
	}
	return func(pool string) (queue.SandboxKey, bool) {
		return q.GetWithStrategy(pool, benchmarkOriginalNodeSpread)
	}
}

type benchmarkNodeSpreadFixture struct {
	keys    []queue.SandboxKey
	updates [2][]queue.SandboxKey
}

func newBenchmarkNodeSpreadFixture(size, nodes int) benchmarkNodeSpreadFixture {
	f := benchmarkNodeSpreadFixture{keys: make([]queue.SandboxKey, size)}
	placements := make([]string, nodes+1)
	for i := range nodes {
		placements[i] = fmt.Sprintf("node-%02d", i)
	}
	for phase := range f.updates {
		f.updates[phase] = make([]queue.SandboxKey, size)
	}
	for i := range f.keys {
		f.keys[i] = queue.SandboxKey{
			Namespace: fmt.Sprintf("tenant-%d", i%2),
			Name:      fmt.Sprintf("sandbox-%04d", i),
			NodeName:  placements[i%nodes],
		}
		if i%7 == 0 {
			f.keys[i].NodeName = ""
		}
		for phase := range f.updates {
			f.updates[phase][i] = f.keys[i]
			f.updates[phase][i].NodeName = placements[(i+phase+1)%len(placements)]
		}
	}
	return f
}

func addBenchmarkNodeSpreadFixture(q *queue.SimpleSandboxQueue, f benchmarkNodeSpreadFixture) {
	for _, key := range f.keys {
		q.Add(benchmarkNodeSpreadPool, key)
	}
}

func benchmarkNodeSpreadOperation(tb testing.TB, q *queue.SimpleSandboxQueue, f benchmarkNodeSpreadFixture, workload string) func(int) queue.SandboxKey {
	get := benchmarkNodeSpreadGetter(q)
	switch workload {
	case "SteadyGetReinsert":
		return func(_ int) queue.SandboxKey {
			key, ok := get(benchmarkNodeSpreadPool)
			if !ok {
				tb.Fatal("NodeSpread claim unexpectedly returned an empty queue")
			}
			q.Add(benchmarkNodeSpreadPool, key)
			return key
		}
	case "NodeChanges":
		return func(i int) queue.SandboxKey {
			key := f.updates[(i/len(f.keys))%2][i%len(f.keys)]
			q.Add(benchmarkNodeSpreadPool, key)
			return key
		}
	case "MixedLifecycle":
		return func(i int) queue.SandboxKey {
			key, ok := get(benchmarkNodeSpreadPool)
			if !ok {
				tb.Fatal("NodeSpread claim unexpectedly returned an empty queue")
			}
			q.Add(benchmarkNodeSpreadPool, key)
			changed := f.updates[(i/len(f.keys))%2][i%len(f.keys)]
			q.Add(benchmarkNodeSpreadPool, changed)
			q.Add(benchmarkNodeSpreadPool, changed)
			replaced := f.keys[(i*7)%len(f.keys)]
			q.RemoveItem(benchmarkNodeSpreadPool, replaced)
			q.Add(benchmarkNodeSpreadPool, replaced)
			oldest, ok := q.Get(benchmarkNodeSpreadPool)
			if !ok {
				tb.Fatal("FIFO claim unexpectedly returned an empty queue")
			}
			q.Add(benchmarkNodeSpreadPool, oldest)
			return key
		}
	case "FIFOGetAdd":
		return func(_ int) queue.SandboxKey {
			key, ok := q.Get(benchmarkNodeSpreadPool)
			if !ok {
				tb.Fatal("FIFO claim unexpectedly returned an empty queue")
			}
			q.Add(benchmarkNodeSpreadPool, key)
			return key
		}
	default:
		panic("unknown benchmark workload: " + workload)
	}
}

func checkBenchmarkNodeSpreadInventory(tb testing.TB, q *queue.SimpleSandboxQueue, f benchmarkNodeSpreadFixture, checkNodes bool) {
	tb.Helper()
	expected := make(map[string]queue.SandboxKey, len(f.keys))
	for _, key := range f.keys {
		expected[key.Namespace+"/"+key.Name] = key
	}
	for {
		key, ok := q.Get(benchmarkNodeSpreadPool)
		if !ok {
			break
		}
		id := key.Namespace + "/" + key.Name
		want, exists := expected[id]
		if !exists || (checkNodes && key != want) {
			tb.Fatalf("unexpected or duplicate key: got %+v, expected %+v, present %t", key, want, exists)
		}
		delete(expected, id)
	}
	if len(expected) != 0 {
		tb.Fatalf("queue lost %d keys", len(expected))
	}
}

func TestNodeSpreadBenchmarkDispatch(t *testing.T) {
	q := queue.NewSimpleSandboxQueue()
	_, indexed := any(q).(interface {
		GetWithNodeSpread(string) (queue.SandboxKey, bool)
	})
	t.Logf("indexed method present: %t", indexed)
	want := queue.SandboxKey{Namespace: "benchmark", Name: "dispatch", NodeName: "node"}
	q.Add(benchmarkNodeSpreadPool, want)
	got, ok := benchmarkNodeSpreadGetter(q)(benchmarkNodeSpreadPool)
	if !ok || got != want {
		t.Fatalf("dispatch got %+v (%t), want %+v", got, ok, want)
	}
}

func TestNodeSpreadBenchmarkFixtures(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		for _, nodes := range []int{1, 8} {
			for _, workload := range []string{"BuildDrain", "SteadyGetReinsert", "NodeChanges", "MixedLifecycle", "FIFOGetAdd"} {
				t.Run(fmt.Sprintf("%s/N%d/Nodes%d", workload, size, nodes), func(t *testing.T) {
					f := newBenchmarkNodeSpreadFixture(size, nodes)
					q := queue.NewSimpleSandboxQueue()
					addBenchmarkNodeSpreadFixture(q, f)
					// The reference uses the real production queue and the original selector.
					reference := queue.NewSimpleSandboxQueue()
					addBenchmarkNodeSpreadFixture(reference, f)
					get := benchmarkNodeSpreadGetter(q)
					referenceGet := func(pool string) (queue.SandboxKey, bool) {
						return reference.GetWithStrategy(pool, benchmarkOriginalNodeSpread)
					}
					if workload != "BuildDrain" {
						step := benchmarkNodeSpreadOperation(t, q, f, workload)
						for i := range 4 * size {
							got := step(i)
							var want queue.SandboxKey
							switch workload {
							case "SteadyGetReinsert", "MixedLifecycle":
								want, _ = referenceGet(benchmarkNodeSpreadPool)
								reference.Add(benchmarkNodeSpreadPool, want)
								if workload == "MixedLifecycle" {
									changed := f.updates[(i/size)%2][i%size]
									reference.Add(benchmarkNodeSpreadPool, changed)
									reference.Add(benchmarkNodeSpreadPool, changed)
									replaced := f.keys[(i*7)%size]
									reference.RemoveItem(benchmarkNodeSpreadPool, replaced)
									reference.Add(benchmarkNodeSpreadPool, replaced)
									oldest, _ := reference.Get(benchmarkNodeSpreadPool)
									reference.Add(benchmarkNodeSpreadPool, oldest)
								}
							case "NodeChanges":
								want = f.updates[(i/size)%2][i%size]
								reference.Add(benchmarkNodeSpreadPool, want)
							case "FIFOGetAdd":
								want, _ = reference.Get(benchmarkNodeSpreadPool)
								reference.Add(benchmarkNodeSpreadPool, want)
							}
							if got != want {
								t.Fatalf("step %d: got %+v, want %+v", i, got, want)
							}
						}
					}
					for i := range size {
						got, ok := get(benchmarkNodeSpreadPool)
						want, wantOK := referenceGet(benchmarkNodeSpreadPool)
						if !ok || !wantOK || got != want {
							t.Fatalf("drain %d: got %+v (%t), want %+v (%t)", i, got, ok, want, wantOK)
						}
					}
					if _, ok := get(benchmarkNodeSpreadPool); ok {
						t.Fatal("queue retained an extra key after drain")
					}
				})
			}
		}
	}
}

func TestNodeSpreadBenchmarkConcurrentFixture(t *testing.T) {
	for _, size := range []int{16, 64, 256, 1024} {
		for _, nodes := range []int{1, 8} {
			t.Run(fmt.Sprintf("N%d/Nodes%d", size, nodes), func(t *testing.T) {
				f := newBenchmarkNodeSpreadFixture(size, nodes)
				q := queue.NewSimpleSandboxQueue()
				addBenchmarkNodeSpreadFixture(q, f)
				get := benchmarkNodeSpreadGetter(q)
				ids := make(map[queue.SandboxKey]int, size)
				for i, key := range f.keys {
					ids[key] = i
				}
				held := make([]atomic.Bool, size)
				var failures atomic.Int64
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						for range 4 * size {
							key, ok := get(benchmarkNodeSpreadPool)
							id, known := ids[key]
							if !ok || !known || !held[id].CompareAndSwap(false, true) {
								failures.Add(1)
								continue
							}
							runtime.Gosched()
							held[id].Store(false)
							q.Add(benchmarkNodeSpreadPool, key)
						}
					})
				}
				workers.Wait()
				if failures.Load() != 0 {
					t.Fatalf("concurrent claim failures: %d", failures.Load())
				}
				checkBenchmarkNodeSpreadInventory(t, q, f, true)
			})
		}
	}
}

func BenchmarkNodeSpreadBuildDrain(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		for _, nodes := range []int{1, 8} {
			b.Run(fmt.Sprintf("N%d/Nodes%d", size, nodes), func(b *testing.B) {
				f := newBenchmarkNodeSpreadFixture(size, nodes)
				var q *queue.SimpleSandboxQueue
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					q = queue.NewSimpleSandboxQueue()
					addBenchmarkNodeSpreadFixture(q, f)
					get := benchmarkNodeSpreadGetter(q)
					for range size {
						if _, ok := get(benchmarkNodeSpreadPool); !ok {
							b.Fatal("NodeSpread claim unexpectedly returned an empty queue")
						}
					}
				}
				b.StopTimer()
				if _, ok := q.Get(benchmarkNodeSpreadPool); ok {
					b.Fatal("queue retained a key after complete drain")
				}
			})
		}
	}
}

func BenchmarkNodeSpreadMutable(b *testing.B) {
	for _, workload := range []string{"SteadyGetReinsert", "NodeChanges", "MixedLifecycle", "FIFOGetAdd"} {
		b.Run(workload, func(b *testing.B) {
			for _, size := range []int{16, 64, 256, 1024} {
				for _, nodes := range []int{1, 8} {
					b.Run(fmt.Sprintf("N%d/Nodes%d", size, nodes), func(b *testing.B) {
						f := newBenchmarkNodeSpreadFixture(size, nodes)
						q := queue.NewSimpleSandboxQueue()
						addBenchmarkNodeSpreadFixture(q, f)
						step := benchmarkNodeSpreadOperation(b, q, f, workload)
						b.ReportAllocs()
						b.ResetTimer()
						for i := range b.N {
							step(i)
						}
						b.StopTimer()
						checkNodes := workload == "SteadyGetReinsert" || workload == "FIFOGetAdd"
						checkBenchmarkNodeSpreadInventory(b, q, f, checkNodes)
					})
				}
			}
		})
	}
}

func BenchmarkNodeSpreadConcurrentClaimRequeue(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		for _, nodes := range []int{1, 8} {
			b.Run(fmt.Sprintf("N%d/Nodes%d", size, nodes), func(b *testing.B) {
				f := newBenchmarkNodeSpreadFixture(size, nodes)
				q := queue.NewSimpleSandboxQueue()
				addBenchmarkNodeSpreadFixture(q, f)
				get := benchmarkNodeSpreadGetter(q)
				var failed atomic.Bool
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						key, ok := get(benchmarkNodeSpreadPool)
						if !ok {
							failed.Store(true)
							return
						}
						q.Add(benchmarkNodeSpreadPool, key)
					}
				})
				b.StopTimer()
				if failed.Load() {
					b.Fatal("concurrent NodeSpread claim unexpectedly returned an empty queue")
				}
				checkBenchmarkNodeSpreadInventory(b, q, f, true)
			})
		}
	}
}
