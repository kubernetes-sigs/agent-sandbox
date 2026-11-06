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
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

// Embedding the original interface hides the optional indexed method.
type strategyOnlyQueue struct {
	queue.SandboxQueue
}

type nodeSpreadAccess struct {
	name string
	pop  func(*queue.SimpleSandboxQueue, string) (queue.SandboxKey, bool)
}

func nodeSpreadAPIs() []nodeSpreadAccess {
	return []nodeSpreadAccess{
		{"method", func(q *queue.SimpleSandboxQueue, pool string) (queue.SandboxKey, bool) {
			return q.GetWithNodeSpread(pool)
		}},
		{"helper", func(q *queue.SimpleSandboxQueue, pool string) (queue.SandboxKey, bool) {
			return queue.GetWithNodeSpread(q, pool)
		}},
		{"fallback", func(q *queue.SimpleSandboxQueue, pool string) (queue.SandboxKey, bool) {
			return queue.GetWithNodeSpread(strategyOnlyQueue{q}, pool)
		}},
	}
}

func spreadKey(name, node string) queue.SandboxKey {
	return queue.SandboxKey{Namespace: "ns", Name: name, NodeName: node}
}

func requireSpreadPop(t *testing.T, got queue.SandboxKey, ok bool, want queue.SandboxKey) {
	t.Helper()
	wantOK := want != (queue.SandboxKey{})
	if ok != wantOK || got != want {
		t.Fatalf("pop = (%+v, %v), want (%+v, %v)", got, ok, want, wantOK)
	}
}

func TestNodeSpreadMutableEdges(t *testing.T) {
	type step struct {
		action string
		key    queue.SandboxKey
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"empty", []step{
			{"spread", queue.SandboxKey{}},
		}},
		{"scheduled-first-and-global-ties", []step{
			{"add", spreadKey("u0", "")},
			{"add", spreadKey("u1", "")},
			{"add", spreadKey("z0", "z")},
			{"add", spreadKey("a0", "a")},
			{"add", spreadKey("a1", "a")},
			{"add", spreadKey("z1", "z")},
			{"spread", spreadKey("z0", "z")},
			{"spread", spreadKey("a0", "a")},
			{"spread", spreadKey("a1", "a")},
			{"spread", spreadKey("z1", "z")},
			{"spread", spreadKey("u0", "")},
			{"spread", spreadKey("u1", "")},
		}},
		{"old-entry-migrates-ahead-of-destination-head", []step{
			{"add", spreadKey("old", "a")},
			{"add", spreadKey("b0", "b")},
			{"add", spreadKey("b1", "b")},
			{"add", spreadKey("a0", "a")},
			{"add", spreadKey("old", "b")},
			{"spread", spreadKey("old", "b")},
			{"spread", spreadKey("b0", "b")},
			{"spread", spreadKey("b1", "b")},
			{"spread", spreadKey("a0", "a")},
		}},
		{"scheduled-unscheduled-transitions", []step{
			{"add", spreadKey("old", "")},
			{"add", spreadKey("b0", "b")},
			{"add", spreadKey("a0", "a")},
			{"add", spreadKey("b1", "b")},
			{"add", spreadKey("old", "a")},
			{"spread", spreadKey("old", "a")},
			{"add", spreadKey("a0", "")},
			{"spread", spreadKey("b0", "b")},
			{"spread", spreadKey("b1", "b")},
			{"spread", spreadKey("a0", "")},
		}},
		{"remove-ignores-node-and-reinsert-resets-age", []step{
			{"add", spreadKey("old", "z")},
			{"add", spreadKey("new", "a")},
			{"remove", spreadKey("old", "stale-node")},
			{"add", spreadKey("old", "z")},
			{"add", spreadKey("old", "z")},
			{"spread", spreadKey("new", "a")},
			{"spread", spreadKey("old", "z")},
		}},
		{"inventory-repairs-after-removal-and-migration", []step{
			{"add", spreadKey("a0", "a")},
			{"add", spreadKey("a1", "a")},
			{"add", spreadKey("b0", "b")},
			{"add", spreadKey("b1", "b")},
			{"add", spreadKey("a2", "a")},
			{"remove", spreadKey("a0", "")},
			{"add", spreadKey("a1", "b")},
			{"spread", spreadKey("a1", "b")},
			{"spread", spreadKey("b0", "b")},
			{"spread", spreadKey("b1", "b")},
			{"spread", spreadKey("a2", "a")},
		}},
		{"mixed-pop-modes-preserve-fifo", []step{
			{"add", spreadKey("u0", "")},
			{"add", spreadKey("a0", "a")},
			{"add", spreadKey("b0", "b")},
			{"add", spreadKey("b1", "b")},
			{"add", spreadKey("u0", "")},
			{"spread", spreadKey("b0", "b")},
			{"fifo", spreadKey("u0", "")},
			{"last", spreadKey("b1", "b")},
			{"add", spreadKey("a0", "c")},
			{"fifo", spreadKey("a0", "c")},
			{"add", spreadKey("fresh", "b")},
			{"spread", spreadKey("fresh", "b")},
		}},
		{"remove-queue-and-recreate", []step{
			{"add", spreadKey("old", "a")},
			{"add", spreadKey("old2", "a")},
			{"clear", queue.SandboxKey{}},
			{"spread", queue.SandboxKey{}},
			{"remove", spreadKey("old", "a")},
			{"add", spreadKey("new", "z")},
			{"add", spreadKey("old", "a")},
			{"spread", spreadKey("new", "z")},
			{"spread", spreadKey("old", "a")},
		}},
	}
	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					q := queue.NewSimpleSandboxQueue()
					for i, s := range tt.steps {
						switch s.action {
						case "add":
							q.Add("ns/pool", s.key)
						case "remove":
							q.RemoveItem("ns/pool", s.key)
						case "clear":
							q.RemoveQueue("ns/pool")
						default:
							var got queue.SandboxKey
							var ok bool
							switch s.action {
							case "fifo":
								got, ok = q.Get("ns/pool")
							case "last":
								got, ok = q.GetWithStrategy("ns/pool", pickLast)
							case "spread":
								got, ok = api.pop(q, "ns/pool")
							default:
								t.Fatalf("unknown action %q", s.action)
							}
							t.Run(fmt.Sprintf("step-%d", i), func(t *testing.T) {
								requireSpreadPop(t, got, ok, s.key)
							})
						}
					}
					got, ok := api.pop(q, "ns/pool")
					requireSpreadPop(t, got, ok, queue.SandboxKey{})
				})
			}
		})
	}
}

func TestNodeSpreadNamespaceAndPoolIsolation(t *testing.T) {
	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			q := queue.NewSimpleSandboxQueue()
			poolA := queue.GetNamespacedWarmPoolName("ns-a", "pool")
			poolB := queue.GetNamespacedWarmPoolName("ns-b", "pool")
			a := queue.SandboxKey{Namespace: "ns-a", Name: "same", NodeName: "z"}
			b := queue.SandboxKey{Namespace: "ns-b", Name: "same", NodeName: "a"}
			q.Add(poolA, a)
			q.Add(poolA, b)
			q.Add(poolB, a)
			q.Add(poolB, b)
			q.RemoveItem(poolA, queue.SandboxKey{Namespace: "ns-a", Name: "same"})
			q.RemoveQueue("pool")
			got, ok := api.pop(q, poolA)
			requireSpreadPop(t, got, ok, b)
			got, ok = api.pop(q, poolA)
			requireSpreadPop(t, got, ok, queue.SandboxKey{})
			q.RemoveQueue(poolA)
			got, ok = api.pop(q, poolB)
			requireSpreadPop(t, got, ok, a)
			got, ok = api.pop(q, poolB)
			requireSpreadPop(t, got, ok, b)
		})
	}
}

func pickLast(items []queue.SandboxKey) (queue.SandboxKey, bool) {
	if len(items) == 0 {
		return queue.SandboxKey{}, false
	}
	return items[len(items)-1], true
}

// The reference stores only FIFO slices and recomputes inventories from scratch.
type spreadModel map[string][]queue.SandboxKey

func sameSandbox(a, b queue.SandboxKey) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

func (m spreadModel) add(pool string, key queue.SandboxKey) {
	for i, item := range m[pool] {
		if sameSandbox(item, key) {
			m[pool][i] = key
			return
		}
	}
	m[pool] = append(m[pool], key)
}

func (m spreadModel) remove(pool string, key queue.SandboxKey) {
	for i, item := range m[pool] {
		if sameSandbox(item, key) {
			m[pool] = slices.Delete(m[pool], i, i+1)
			return
		}
	}
}

func (m spreadModel) pop(pool, mode string) (queue.SandboxKey, bool) {
	items := m[pool]
	if len(items) == 0 {
		return queue.SandboxKey{}, false
	}
	chosen := 0
	switch mode {
	case "spread":
		bestCount := 0
		for i, item := range items {
			if item.NodeName == "" {
				continue
			}
			count := 0
			for _, other := range items {
				if item.NodeName == other.NodeName {
					count++
				}
			}
			if count > bestCount {
				chosen, bestCount = i, count
			}
		}
	case "last":
		chosen = len(items) - 1
	}
	key := items[chosen]
	m[pool] = slices.Delete(items, chosen, chosen+1)
	return key, true
}

func queueSnapshot(q queue.SandboxQueue, pool string) []queue.SandboxKey {
	var snapshot []queue.SandboxKey
	q.GetWithStrategy(pool, func(items []queue.SandboxKey) (queue.SandboxKey, bool) {
		snapshot = slices.Clone(items)
		return queue.SandboxKey{}, false
	})
	return snapshot
}

func runMutableSpreadModel(t *testing.T, data []byte, api nodeSpreadAccess) {
	t.Helper()
	q := queue.NewSimpleSandboxQueue()
	model := spreadModel{}
	pools := []string{"ns-a/pool", "ns-b/pool", "ns-a/other"}
	nodes := []string{"", "a", "b", "c", "z"}
	for _, pool := range pools {
		for i := range 24 {
			key := queue.SandboxKey{Namespace: fmt.Sprintf("ns-%d", i%2), Name: fmt.Sprintf("sb-%d", i/2), NodeName: nodes[i%len(nodes)]}
			q.Add(pool, key)
			model.add(pool, key)
		}
	}
	for offset := 0; offset+5 <= len(data); offset += 5 {
		op := data[offset : offset+5]
		pool := pools[int(op[1])%len(pools)]
		key := queue.SandboxKey{
			Namespace: fmt.Sprintf("ns-%d", op[2]%2),
			Name:      fmt.Sprintf("sb-%d", op[3]%16),
			NodeName:  nodes[int(op[4])%len(nodes)],
		}
		var got, want queue.SandboxKey
		var ok, wantOK bool
		switch action := op[0] % 24; {
		case action < 10:
			q.Add(pool, key)
			model.add(pool, key)
		case action < 12:
			q.RemoveItem(pool, key)
			model.remove(pool, key)
		case action < 14:
			got, ok = q.Get(pool)
			want, wantOK = model.pop(pool, "fifo")
		case action < 18:
			got, ok = api.pop(q, pool)
			want, wantOK = model.pop(pool, "spread")
		case action < 20:
			got, ok = q.GetWithStrategy(pool, pickLast)
			want, wantOK = model.pop(pool, "last")
		case action < 22:
			q.RemoveItem(pool, key)
			model.remove(pool, key)
			q.Add(pool, key)
			model.add(pool, key)
		case action == 22:
			q.RemoveQueue(pool)
			delete(model, pool)
		default:
			got, ok = q.GetWithStrategy(pool, func(items []queue.SandboxKey) (queue.SandboxKey, bool) {
				if len(items) > 0 {
					items[0] = spreadKey("snapshot-only", "snapshot-only")
				}
				return queue.SandboxKey{}, false
			})
		}
		if got != want || ok != wantOK {
			t.Fatalf("step %d, operation %v, pool %q: got (%+v, %v), want (%+v, %v)",
				offset/5, op, pool, got, ok, want, wantOK)
		}
		for _, pool := range pools {
			snapshot := queueSnapshot(q, pool)
			if !slices.Equal(snapshot, model[pool]) {
				t.Fatalf("step %d, operation %v, pool %q: snapshot = %+v, want %+v",
					offset/5, op, pool, snapshot, model[pool])
			}
		}
	}
	for _, pool := range pools {
		for len(model[pool]) > 0 {
			got, ok := api.pop(q, pool)
			want, wantOK := model.pop(pool, "spread")
			if got != want || ok != wantOK {
				t.Fatalf("final drain %q: got (%+v, %v), want (%+v, %v)", pool, got, ok, want, wantOK)
			}
		}
		got, ok := api.pop(q, pool)
		requireSpreadPop(t, got, ok, queue.SandboxKey{})
	}
}

func TestNodeSpreadMutableModel(t *testing.T) {
	data := make([]byte, 5*4000)
	_, _ = rand.New(rand.NewSource(0)).Read(data)
	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			runMutableSpreadModel(t, data, api)
		})
	}
}

func FuzzNodeSpreadMutable(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 1, 0, 0, 0, 0, 2, 14, 0, 0, 0, 0, 22, 0, 0, 0, 0})
	data := make([]byte, 5*128)
	_, _ = rand.New(rand.NewSource(0)).Read(data)
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 5*512 {
			data = data[:5*512]
		}
		for _, api := range nodeSpreadAPIs() {
			t.Run(api.name, func(t *testing.T) {
				runMutableSpreadModel(t, data, api)
			})
		}
	})
}

func TestNodeSpreadStrategySnapshotReentrancy(t *testing.T) {
	a := spreadKey("a", "old-node")
	b := spreadKey("b", "b")
	updated := spreadKey("a", "new-node")
	tests := []struct {
		name      string
		mutate    func(*queue.SimpleSandboxQueue)
		want      queue.SandboxKey
		remaining []queue.SandboxKey
		calls     int
	}{
		{"remove-selected", func(q *queue.SimpleSandboxQueue) {
			q.RemoveItem("pool", spreadKey("a", "stale-node"))
		}, b, nil, 2},
		{"update-selected", func(q *queue.SimpleSandboxQueue) {
			q.Add("pool", updated)
		}, a, []queue.SandboxKey{b}, 1},
		{"remove-and-reinsert-selected", func(q *queue.SimpleSandboxQueue) {
			q.RemoveItem("pool", a)
			q.Add("pool", updated)
		}, a, []queue.SandboxKey{b}, 1},
		{"remove-pool-and-recreate", func(q *queue.SimpleSandboxQueue) {
			q.RemoveQueue("pool")
			q.Add("pool", updated)
		}, a, []queue.SandboxKey{updated}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := queue.NewSimpleSandboxQueue()
			q.Add("pool", a)
			q.Add("pool", b)
			type selection struct {
				key       queue.SandboxKey
				ok        bool
				snapshots [][]queue.SandboxKey
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			selected := make(chan selection, 1)
			go func() {
				var snapshots [][]queue.SandboxKey
				key, ok := q.GetWithStrategy("pool", func(items []queue.SandboxKey) (queue.SandboxKey, bool) {
					snapshots = append(snapshots, slices.Clone(items))
					if len(snapshots) == 1 {
						close(entered)
						<-release
					}
					if len(snapshots) > 2 {
						return queue.SandboxKey{}, false
					}
					return items[0], true
				})
				selected <- selection{key, ok, snapshots}
			}()
			// Mutations must finish while the callback still owns its snapshot.
			<-entered
			mutated := make(chan struct{})
			go func() {
				tt.mutate(q)
				close(mutated)
			}()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-mutated:
			case <-timer.C:
				t.Error("queue mutation blocked while the strategy callback was running")
			}
			close(release)
			<-mutated
			result := <-selected
			requireSpreadPop(t, result.key, result.ok, tt.want)
			if first := result.snapshots[0]; !slices.Equal(first, []queue.SandboxKey{a, b}) {
				t.Errorf("initial snapshot = %+v, want [%+v %+v]", first, a, b)
			}
			if len(result.snapshots) != tt.calls {
				t.Errorf("callback calls = %d, want %d", len(result.snapshots), tt.calls)
			}
			if len(result.snapshots) == 2 {
				if second := result.snapshots[1]; !slices.Equal(second, []queue.SandboxKey{b}) {
					t.Errorf("retry snapshot = %+v, want [%+v]", second, b)
				}
			}
			if remaining := queueSnapshot(q, "pool"); !slices.Equal(remaining, tt.remaining) {
				t.Errorf("remaining = %+v, want %+v", remaining, tt.remaining)
			}
		})
	}
}

func TestNodeSpreadStrategySnapshotContention(t *testing.T) {
	const workers = 8
	apis := append(nodeSpreadAPIs(), nodeSpreadAccess{
		"fifo", func(q *queue.SimpleSandboxQueue, pool string) (queue.SandboxKey, bool) {
			return q.Get(pool)
		},
	})
	for _, api := range apis {
		t.Run(api.name, func(t *testing.T) {
			q := queue.NewSimpleSandboxQueue()
			want := make(map[queue.SandboxKey]bool, workers+1)
			for i := range workers + 1 {
				key := spreadKey(fmt.Sprintf("sb-%d", i), "node")
				q.Add("pool", key)
				want[key] = true
			}
			type selection struct {
				key   queue.SandboxKey
				ok    bool
				calls int
			}
			entered := make(chan queue.SandboxKey, workers)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			results := make(chan selection, workers)
			for range workers {
				go func() {
					calls := 0
					key, ok := q.GetWithStrategy("pool", func(items []queue.SandboxKey) (queue.SandboxKey, bool) {
						calls++
						if calls == 1 {
							entered <- items[0]
							<-release
						}
						if calls > workers+1 {
							return queue.SandboxKey{}, false
						}
						return items[0], true
					})
					results <- selection{key, ok, calls}
				}()
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			first := spreadKey("sb-0", "node")
			for range workers {
				select {
				case key := <-entered:
					if key != first {
						t.Fatalf("initial snapshot selected %+v, want %+v", key, first)
					}
				case <-timer.C:
					t.Fatal("strategy callbacks did not all acquire their snapshots")
				}
			}

			// Remove the shared first choice while every callback still holds its snapshot.
			removed := make(chan selection, 1)
			go func() {
				key, ok := api.pop(q, "pool")
				removed <- selection{key: key, ok: ok}
			}()
			select {
			case result := <-removed:
				requireSpreadPop(t, result.key, result.ok, first)
				delete(want, first)
			case <-timer.C:
				t.Fatal("pop blocked while strategy callbacks were running")
			}
			unblock()
			for range workers {
				select {
				case result := <-results:
					if !result.ok || !want[result.key] {
						t.Fatalf("duplicate or unexpected snapshot selection: %+v", result)
					}
					if result.calls < 2 || result.calls > workers+1 {
						t.Fatalf("callback calls = %d, want between 2 and %d", result.calls, workers+1)
					}
					delete(want, result.key)
				case <-timer.C:
					t.Fatal("strategy consumers did not finish retrying stale snapshots")
				}
			}
			if len(want) != 0 {
				t.Fatalf("%d sandboxes were not drained", len(want))
			}
			got, ok := api.pop(q, "pool")
			requireSpreadPop(t, got, ok, queue.SandboxKey{})
		})
	}
}

func TestNodeSpreadStrategySnapshotInventoryChange(t *testing.T) {
	a := spreadKey("a", "node-a")
	b := spreadKey("b", "node-b")
	c := spreadKey("c", "node-b")
	q := queue.NewSimpleSandboxQueue()
	q.Add("pool", a)
	q.Add("pool", b)
	calls := 0
	got, ok := q.GetWithStrategy("pool", func(items []queue.SandboxKey) (queue.SandboxKey, bool) {
		calls++
		q.Add("pool", c)
		if !slices.Equal(items, []queue.SandboxKey{a, b}) {
			t.Fatalf("snapshot changed after Add: %+v", items)
		}
		model := spreadModel{"pool": items}
		return model.pop("pool", "spread")
	})
	// A surviving identity is accepted even when newer inventory changes its priority.
	requireSpreadPop(t, got, ok, a)
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if remaining := queueSnapshot(q, "pool"); !slices.Equal(remaining, []queue.SandboxKey{b, c}) {
		t.Fatalf("remaining = %+v, want [%+v %+v]", remaining, b, c)
	}

	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			current := queue.NewSimpleSandboxQueue()
			for _, key := range []queue.SandboxKey{a, b, c} {
				current.Add("pool", key)
			}
			got, ok := api.pop(current, "pool")
			requireSpreadPop(t, got, ok, b)
		})
	}
}

func TestNodeSpreadConcurrentDrainUnique(t *testing.T) {
	const count, workers = 256, 8
	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			q := queue.NewSimpleSandboxQueue()
			want := make(map[queue.SandboxKey]bool, count)
			for i := range count {
				key := queue.SandboxKey{Namespace: fmt.Sprintf("ns-%d", i%2), Name: fmt.Sprintf("sb-%d", i/2)}
				if i%5 != 0 {
					key.NodeName = fmt.Sprintf("node-%d", i%7)
				}
				q.Add("pool", key)
				want[key] = true
			}
			results := make(chan queue.SandboxKey, workers*(count+1))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for worker := range workers {
				wg.Go(func() {
					<-start
					for range count + 1 {
						var key queue.SandboxKey
						var ok bool
						switch worker % 3 {
						case 0:
							key, ok = q.Get("pool")
						case 1:
							key, ok = api.pop(q, "pool")
						default:
							key, ok = q.GetWithStrategy("pool", pickLast)
						}
						if !ok {
							return
						}
						results <- key
					}
				})
			}
			close(start)
			wg.Wait()
			close(results)
			for key := range results {
				if !want[key] {
					t.Errorf("duplicate or unexpected sandbox: %+v", key)
				}
				delete(want, key)
			}
			if len(want) != 0 {
				t.Errorf("%d sandboxes were not drained", len(want))
			}
			got, ok := api.pop(q, "pool")
			requireSpreadPop(t, got, ok, queue.SandboxKey{})
		})
	}
}

func TestNodeSpreadConcurrentMutations(t *testing.T) {
	const workers, iterations = 8, 256
	for _, api := range nodeSpreadAPIs() {
		t.Run(api.name, func(t *testing.T) {
			q := queue.NewSimpleSandboxQueue()
			pools := []string{"ns-a/pool", "ns-b/pool", "ns-a/other"}
			valid := make(map[queue.SandboxKey]bool)
			for i := range 32 {
				for _, node := range []string{"", "a", "b", "c"} {
					valid[queue.SandboxKey{Namespace: fmt.Sprintf("ns-%d", i%2), Name: fmt.Sprintf("sb-%d", i/2), NodeName: node}] = true
				}
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			for worker := range workers {
				wg.Go(func() {
					<-start
					for i := range iterations {
						index := (i*7 + worker*3) % 32
						pool := pools[(i/3+worker*2)%len(pools)]
						key := queue.SandboxKey{
							Namespace: fmt.Sprintf("ns-%d", index%2),
							Name:      fmt.Sprintf("sb-%d", index/2),
							NodeName:  []string{"", "a", "b", "c"}[(i+worker)%4],
						}
						var got queue.SandboxKey
						var ok bool
						switch (i + worker) % 9 {
						case 0, 1, 2:
							q.Add(pool, key)
						case 3:
							q.RemoveItem(pool, key)
						case 4:
							q.RemoveItem(pool, key)
							q.Add(pool, key)
						case 5:
							q.RemoveQueue(pool)
						case 6:
							got, ok = q.Get(pool)
						case 7:
							got, ok = api.pop(q, pool)
						case 8:
							got, ok = q.GetWithStrategy(pool, pickLast)
						}
						if ok && !valid[got] {
							t.Errorf("unexpected sandbox from concurrent pop: %+v", got)
						}
					}
				})
			}
			close(start)
			wg.Wait()
			for _, pool := range pools {
				snapshot := queueSnapshot(q, pool)
				seen := make(map[[2]string]bool)
				for _, key := range snapshot {
					id := [2]string{key.Namespace, key.Name}
					if seen[id] || !valid[key] {
						t.Fatalf("duplicate or unexpected remaining sandbox: %+v", key)
					}
					seen[id] = true
				}
				model := spreadModel{pool: snapshot}
				for len(model[pool]) > 0 {
					want, _ := model.pop(pool, "spread")
					got, ok := api.pop(q, pool)
					requireSpreadPop(t, got, ok, want)
				}
				got, ok := api.pop(q, pool)
				requireSpreadPop(t, got, ok, queue.SandboxKey{})
				q.RemoveQueue(pool)
				fresh := spreadKey("fresh", "fresh-node")
				q.Add(pool, fresh)
				got, ok = api.pop(q, pool)
				requireSpreadPop(t, got, ok, fresh)
			}
		})
	}
}
