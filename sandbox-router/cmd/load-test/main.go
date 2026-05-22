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

// Binary load-test is a standalone harness that measures the ext-proc
// service under realistic-ish load: a sandbox pool of configurable size,
// continuous churn driven through the fake K8s informer path, and
// concurrent gRPC clients sending ProcessingRequest messages against the
// real Server. It reports throughput, latency percentiles, cache hit
// ratio, and memory usage.
//
// Everything runs in-process. There is no real Kubernetes cluster and no
// Envoy — the goal is to characterize the SERVER's capacity, not the
// cluster end-to-end. For end-to-end testing, run this binary against a
// kind cluster with --listen-address instead of in-process.
//
// Usage:
//
//	go run ./sandbox-router/cmd/load-test \
//	    --sandboxes=5000 --churn-rate=100 \
//	    --clients=64 --duration=30s --cache-hit-pct=80
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	routercache "sigs.k8s.io/agent-sandbox/sandbox-router/internal/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/internal/extproc"
)

func main() {
	var (
		sandboxes     int
		churnRate     int
		clients       int
		duration      time.Duration
		warmup        time.Duration
		cacheHitPct   int
		clusterDomain string
		listenAddr    string
	)
	flag.IntVar(&sandboxes, "sandboxes", 1000,
		"Target sandbox pool size (cache entries maintained throughout the run).")
	flag.IntVar(&churnRate, "churn-rate", 0,
		"Add+remove operations per second on the informer. 0 = static pool.")
	flag.IntVar(&clients, "clients", 32,
		"Number of concurrent gRPC clients. Each runs a tight Send/Recv loop on its own stream.")
	flag.DurationVar(&duration, "duration", 15*time.Second,
		"How long to drive load before stopping.")
	flag.DurationVar(&warmup, "warmup", 2*time.Second,
		"Warmup window during which samples are NOT recorded; lets the JIT, cache, and informer settle.")
	flag.IntVar(&cacheHitPct, "cache-hit-pct", 80,
		"Percentage of requests that should target a known UID. The rest use random UIDs (cache miss → DNS fallback).")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local",
		"Cluster domain passed to the extproc server (only affects the DNS-form fallback target).")
	flag.StringVar(&listenAddr, "listen-address", "127.0.0.1:0",
		"TCP address for the in-process gRPC server. 127.0.0.1:0 picks a free port.")
	flag.Parse()

	if cacheHitPct < 0 || cacheHitPct > 100 {
		fmt.Fprintln(os.Stderr, "--cache-hit-pct must be in [0,100]")
		os.Exit(2)
	}

	if err := run(runConfig{
		sandboxes:     sandboxes,
		churnRate:     churnRate,
		clients:       clients,
		duration:      duration,
		warmup:        warmup,
		cacheHitPct:   cacheHitPct,
		clusterDomain: clusterDomain,
		listenAddr:    listenAddr,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "load-test failed: %v\n", err)
		os.Exit(1)
	}
}

type runConfig struct {
	sandboxes     int
	churnRate     int
	clients       int
	duration      time.Duration
	warmup        time.Duration
	cacheHitPct   int
	clusterDomain string
	listenAddr    string
}

func run(cfg runConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration+cfg.warmup+10*time.Second)
	defer cancel()

	// ---- pre-populate sandboxes ----
	initial := make([]*corev1.Pod, 0, cfg.sandboxes)
	for i := range cfg.sandboxes {
		initial = append(initial, makePod(i))
	}
	objs := make([]kruntime.Object, len(initial))
	for i, p := range initial {
		objs[i] = p
	}
	client := fake.NewSimpleClientset(objs...)

	// ---- start cache + server ----
	cch, err := routercache.New(routercache.Options{Client: client, Log: logr.Discard()})
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	cch.Start(ctx)
	if !cch.WaitForSync(ctx) {
		return fmt.Errorf("informer sync timed out")
	}

	srv, err := extproc.NewServer(extproc.Options{
		Cache:         cch,
		ClusterDomain: cfg.clusterDomain,
		Log:           logr.Discard(),
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	lis, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer lis.Close()
	gsrv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	defer gsrv.Stop()

	// ---- pool of "known" UIDs the clients will hit ----
	pool := newUIDPool(initial)

	// ---- churn ----
	churnDone := make(chan struct{})
	if cfg.churnRate > 0 {
		go runChurn(ctx, client, pool, cfg.churnRate, churnDone)
	} else {
		close(churnDone)
	}

	// ---- start clients ----
	addr := lis.Addr().String()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	stats := &statsCollector{}
	startTime := time.Now()
	recordingStart := startTime.Add(cfg.warmup)
	stopAt := startTime.Add(cfg.warmup + cfg.duration)

	wg := sync.WaitGroup{}
	wg.Add(cfg.clients)
	for range cfg.clients {
		go func() {
			defer wg.Done()
			driveClient(ctx, conn, pool, cfg.cacheHitPct, recordingStart, stopAt, stats)
		}()
	}

	// Wait either for stopAt + a small grace, or for ctx cancel.
	deadline := time.NewTimer(time.Until(stopAt) + 200*time.Millisecond)
	defer deadline.Stop()
	select {
	case <-deadline.C:
	case <-ctx.Done():
	}
	cancel() // signal churn + clients to stop
	wg.Wait()
	<-churnDone

	// ---- final memory snapshot ----
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	stats.report(cfg, cch.Len(), mem)
	return nil
}

// ---- fake objects + pool --------------------------------------------------

// makePod constructs a synthetic sandbox Pod with a unique UID, name, and
// IP. Looks indistinguishable to the cache from a real controller-created
// Pod. Index i is encoded into the IP for debuggability.
func makePod(i int) *corev1.Pod {
	tru := true
	name := fmt.Sprintf("sb-%06d", i)
	uid := randomUID()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenants",
			Labels:    map[string]string{routercache.PodSandboxNameHashLabel: "loadtest"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: routercache.SandboxAPIGroup + "/v1beta1",
				Kind:       routercache.SandboxKind,
				Name:       name,
				UID:        uid,
				Controller: &tru,
			}},
		},
		Status: corev1.PodStatus{
			PodIP: fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff),
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func randomUID() types.UID {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return types.UID(hex.EncodeToString(b))
}

// uidPool is the set of currently-known (cached) sandboxes. Clients pick
// random members of this pool when they want to generate cache-hit
// traffic. Churn mutates it.
type uidPool struct {
	mu  sync.RWMutex
	ids []poolEntry
}

type poolEntry struct {
	name      string
	namespace string
	uid       types.UID
}

func newUIDPool(pods []*corev1.Pod) *uidPool {
	p := &uidPool{ids: make([]poolEntry, 0, len(pods))}
	for _, pod := range pods {
		if len(pod.OwnerReferences) == 0 {
			continue
		}
		p.ids = append(p.ids, poolEntry{
			name:      pod.Name,
			namespace: pod.Namespace,
			uid:       pod.OwnerReferences[0].UID,
		})
	}
	return p
}

func (p *uidPool) pickHit() (poolEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.ids) == 0 {
		return poolEntry{}, false
	}
	return p.ids[mathrand.IntN(len(p.ids))], true
}

func (p *uidPool) replace(remove, add poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) == 0 {
		return
	}
	// Find remove (O(N); pool size is bounded by sandbox count, fine).
	idx := slices.IndexFunc(p.ids, func(e poolEntry) bool { return e.uid == remove.uid })
	if idx >= 0 {
		p.ids[idx] = add
	}
}

// ---- churn ---------------------------------------------------------------

// runChurn drives add+remove operations at rate ops/sec, keeping the
// sandbox pool size constant. Each "op" is one remove + one add (a
// replacement), so churn-rate=100 = 100 replacements/s = ~200 informer
// events/s.
func runChurn(ctx context.Context, client *fake.Clientset, pool *uidPool, ratePerSec int, done chan<- struct{}) {
	defer close(done)
	if ratePerSec <= 0 {
		return
	}
	interval := time.Second / time.Duration(ratePerSec)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	next := mathrand.Uint64() // for fresh sandbox indices

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Pick a victim to remove.
			pool.mu.Lock()
			if len(pool.ids) == 0 {
				pool.mu.Unlock()
				continue
			}
			victimIdx := mathrand.IntN(len(pool.ids))
			victim := pool.ids[victimIdx]
			pool.mu.Unlock()

			// Spawn a new sandbox to replace it. Different name and UID so
			// the informer sees both a Delete (or Update if reused) and an
			// Add — exercising both event paths.
			next++
			newPod := makePod(int(next))
			newEntry := poolEntry{
				name:      newPod.Name,
				namespace: newPod.Namespace,
				uid:       newPod.OwnerReferences[0].UID,
			}

			_ = client.CoreV1().Pods(victim.namespace).Delete(ctx, victim.name, metav1.DeleteOptions{})
			_, _ = client.CoreV1().Pods(newPod.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
			pool.replace(victim, newEntry)
		}
	}
}

// ---- clients -------------------------------------------------------------

// driveClient opens a single ext_proc stream and pumps requests through
// it in a tight loop. Latency is recorded only for samples taken inside
// the recording window (after warmup, before stop).
func driveClient(ctx context.Context, conn *grpc.ClientConn, pool *uidPool, hitPct int,
	recordStart, stopAt time.Time, stats *statsCollector,
) {
	cli := extprocv3.NewExternalProcessorClient(conn)
	stream, err := cli.Process(ctx)
	if err != nil {
		stats.errors.Add(1)
		return
	}
	defer func() { _ = stream.CloseSend() }()

	for {
		now := time.Now()
		if !now.Before(stopAt) || ctx.Err() != nil {
			return
		}
		req := buildRequest(pool, hitPct)

		t0 := time.Now()
		if err := stream.Send(req); err != nil {
			stats.errors.Add(1)
			return
		}
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			stats.errors.Add(1)
			return
		}
		latency := time.Since(t0)

		hit := isCacheHit(resp)
		if now.After(recordStart) {
			stats.record(latency, hit)
		}
	}
}

func buildRequest(pool *uidPool, hitPct int) *extprocv3.ProcessingRequest {
	var (
		id        string
		uid       string
		namespace string
	)
	if mathrand.IntN(100) < hitPct {
		if e, ok := pool.pickHit(); ok {
			id = e.name
			uid = string(e.uid)
			namespace = e.namespace
		}
	}
	if id == "" {
		// Cache-miss path: random sandbox id, random UID; expected to hit
		// DNS fallback in the server.
		id = "miss-" + hex.EncodeToString(randomBytes(4))
		uid = string(randomUID())
		namespace = "tenants"
	}
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{
					Headers: []*corev3.HeaderValue{
						{Key: extproc.HeaderSandboxID, RawValue: []byte(id)},
						{Key: extproc.HeaderSandboxUID, RawValue: []byte(uid)},
						{Key: extproc.HeaderSandboxNamespace, RawValue: []byte(namespace)},
						{Key: extproc.HeaderSandboxPort, RawValue: []byte("8888")},
					},
				},
			},
		},
	}
}

// isCacheHit inspects the server's HeaderMutation. We tagged cache vs
// DNS by inspecting whether the target ends with the cluster suffix —
// the server emits IP:port for cache hits and "<id>.<ns>.svc.cluster.local:port"
// for DNS fallback.
func isCacheHit(resp *extprocv3.ProcessingResponse) bool {
	hr := resp.GetRequestHeaders()
	if hr == nil || hr.Response == nil || hr.Response.HeaderMutation == nil {
		return false
	}
	for _, h := range hr.Response.HeaderMutation.SetHeaders {
		if h.Header.Key != extproc.HeaderOriginalDstHost {
			continue
		}
		v := string(h.Header.RawValue)
		// DNS form contains ".svc."; cache hits are bare IP:port.
		for i := 0; i+5 <= len(v); i++ {
			if v[i:i+5] == ".svc." {
				return false
			}
		}
		return true
	}
	return false
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// ---- stats ---------------------------------------------------------------

type statsCollector struct {
	mu        sync.Mutex
	latencies []time.Duration
	hits      atomic.Int64
	misses    atomic.Int64
	errors    atomic.Int64
}

func (s *statsCollector) record(latency time.Duration, hit bool) {
	s.mu.Lock()
	s.latencies = append(s.latencies, latency)
	s.mu.Unlock()
	if hit {
		s.hits.Add(1)
	} else {
		s.misses.Add(1)
	}
}

func (s *statsCollector) report(cfg runConfig, finalCacheSize int, mem runtime.MemStats) {
	s.mu.Lock()
	lats := slices.Clone(s.latencies)
	s.mu.Unlock()
	slices.Sort(lats)

	total := int64(len(lats))
	hits := s.hits.Load()
	misses := s.misses.Load()
	errs := s.errors.Load()

	fmt.Println()
	fmt.Println("=== ext-proc load test ===")
	fmt.Printf("  sandboxes (target):  %d\n", cfg.sandboxes)
	fmt.Printf("  cache size (final):  %d\n", finalCacheSize)
	fmt.Printf("  churn rate:          %d ops/s\n", cfg.churnRate)
	fmt.Printf("  clients:             %d\n", cfg.clients)
	fmt.Printf("  duration (recorded): %s  (+ warmup %s)\n", cfg.duration, cfg.warmup)
	fmt.Printf("  cache-hit target:    %d%%\n", cfg.cacheHitPct)
	fmt.Println()
	fmt.Println("  --- throughput ---")
	if cfg.duration > 0 {
		fmt.Printf("    requests recorded: %d\n", total)
		fmt.Printf("    throughput:        %.0f req/s\n", float64(total)/cfg.duration.Seconds())
	}
	fmt.Println("  --- outcomes ---")
	if hits+misses > 0 {
		fmt.Printf("    cache hits:        %d (%.1f%%)\n", hits, 100*float64(hits)/float64(hits+misses))
		fmt.Printf("    cache misses:      %d (%.1f%%)\n", misses, 100*float64(misses)/float64(hits+misses))
	}
	fmt.Printf("    errors:            %d\n", errs)
	fmt.Println("  --- latency (request → response on the gRPC stream) ---")
	if len(lats) == 0 {
		fmt.Println("    (no samples recorded)")
	} else {
		fmt.Printf("    p50:   %s\n", pct(lats, 50))
		fmt.Printf("    p90:   %s\n", pct(lats, 90))
		fmt.Printf("    p95:   %s\n", pct(lats, 95))
		fmt.Printf("    p99:   %s\n", pct(lats, 99))
		fmt.Printf("    p999:  %s\n", pct(lats, 99.9))
		fmt.Printf("    max:   %s\n", lats[len(lats)-1].Round(time.Microsecond))
	}
	fmt.Println("  --- memory (final) ---")
	fmt.Printf("    heap in-use:       %s\n", humanBytes(mem.HeapInuse))
	fmt.Printf("    heap allocated:    %s\n", humanBytes(mem.HeapAlloc))
	fmt.Printf("    GC cycles:         %d\n", mem.NumGC)
	fmt.Println()
}

func pct(s []time.Duration, p float64) time.Duration {
	if len(s) == 0 {
		return 0
	}
	idx := int(float64(len(s)) * p / 100)
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx].Round(time.Microsecond)
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
