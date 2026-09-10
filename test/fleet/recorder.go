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
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// recorder writes duckdb-friendly artifacts:
//   - creates-<cluster>.jsonl.gz: one row per create (id, ack latency)
//   - ready-<cluster>.jsonl.gz:   one row per first Ready observation
//     (client clock + the Ready condition's server-side transition time)
//   - summary.json:               fleet verdict, per-cluster totals
//
// Raw jsonl is the source of truth; the verdict is recomputed offline
// from server-side stamps (loud-failure philosophy: if summary and raw
// rows disagree, the rows win).
// recorder is sharded per cluster: each shard has its own mutex, writers,
// dedup set, and ready-stamp slice. The previous single global mutex
// serialized EVERY cluster's record path - and because the buffered gzip
// writer runs Deflate compression on flush while the lock is held, one
// cluster's periodic 1MB flush stalled all 18 clusters' recording on a
// single core. Per-cluster shards make recording embarrassingly parallel;
// the summary merges the shards' stamps once at the end.
type recorder struct {
	shards []*recShard
	outDir string
	errs   atomic.Int64
}

type recShard struct {
	mu      sync.Mutex
	creates *gzWriter
	readys  *gzWriter
	seen    map[string]struct{} // sandboxes already recorded Ready
	readyTs []time.Time         // server-side ready stamps for this cluster
}

type gzWriter struct {
	f  *os.File
	gz *gzip.Writer
	bw *bufio.Writer
}

func newGzWriter(path string) (*gzWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	return &gzWriter{f: f, gz: gz, bw: bufio.NewWriterSize(gz, 1<<20)}, nil
}

func (w *gzWriter) Close() error {
	w.bw.Flush()
	w.gz.Close()
	return w.f.Close()
}

func newRecorder(outDir string, clusters []*cluster) (*recorder, error) {
	r := &recorder{
		shards: make([]*recShard, len(clusters)),
		outDir: outDir,
	}
	for _, c := range clusters {
		cw, err := newGzWriter(filepath.Join(outDir, fmt.Sprintf("creates-%s.jsonl.gz", c.Name)))
		if err != nil {
			return nil, err
		}
		rw, err := newGzWriter(filepath.Join(outDir, fmt.Sprintf("ready-%s.jsonl.gz", c.Name)))
		if err != nil {
			return nil, err
		}
		r.shards[c.Index] = &recShard{
			creates: cw,
			readys:  rw,
			seen:    map[string]struct{}{},
		}
	}
	return r, nil
}

func (r *recorder) RecordCreate(c *cluster, id int, at time.Time, ack time.Duration) {
	row, _ := json.Marshal(map[string]any{
		"id":      id,
		"cluster": c.Name,
		"created": at.UTC().Format(time.RFC3339Nano),
		"ackMs":   float64(ack.Microseconds()) / 1000.0,
	})
	s := r.shards[c.Index]
	s.mu.Lock()
	s.creates.bw.Write(row)
	s.creates.bw.WriteByte('\n')
	s.mu.Unlock()
}

func (r *recorder) RecordCreateError(c *cluster, id int, err error) {
	n := r.errs.Add(1)
	if n <= 20 || n%1000 == 0 {
		log.Printf("[%s] create %d failed (%d total errors): %v", c.Name, id, n, err)
	}
}

// RecordReady records the FIRST Ready observation for a sandbox;
// returns false for duplicates (watch replays after reconnect).
func (r *recorder) RecordReady(c *cluster, name string, observed time.Time, serverReady time.Time) bool {
	s := r.shards[c.Index]
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.seen[name]; dup {
		return false
	}
	s.seen[name] = struct{}{}
	s.readyTs = append(s.readyTs, serverReady)
	row, _ := json.Marshal(map[string]any{
		"name":        name,
		"cluster":     c.Name,
		"observed":    observed.UTC().Format(time.RFC3339Nano),
		"serverReady": serverReady.UTC().Format(time.RFC3339),
	})
	s.readys.bw.Write(row)
	s.readys.bw.WriteByte('\n')
	return true
}

func (r *recorder) ReadyTotal() int {
	total := 0
	for _, s := range r.shards {
		s.mu.Lock()
		total += len(s.readyTs)
		s.mu.Unlock()
	}
	return total
}

func (r *recorder) Close() {
	for _, s := range r.shards {
		s.mu.Lock()
		s.creates.Close()
		s.readys.Close()
		s.mu.Unlock()
	}
}

// WriteSummary computes the fleet verdict from server-side Ready stamps:
// the best 60s window across the whole fleet, plus per-cluster totals.
func (r *recorder) WriteSummary(cfg config, clusters []*cluster, start time.Time) error {
	var stamps []time.Time
	for _, s := range r.shards {
		s.mu.Lock()
		stamps = append(stamps, s.readyTs...)
		s.mu.Unlock()
	}
	errs := int(r.errs.Load())

	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })

	// Two windows, both over server-side Ready stamps (one fleet-wide
	// window, never per-cluster maxima summed):
	//   - fixedWindow60s: a FIXED 60s window starting 10s after the first
	//     Ready, skipping the ramp. This is the HONEST headline - it does
	//     not slide to find a peak, so it cannot flatter the number.
	//   - bestServer60s: the sliding max over all 60s windows. Reported for
	//     reference; it runs a few % higher than the fixed window (61,025
	//     vs 58,640 on a single n2-standard-4 cluster), and that gap is
	//     enough to flip a MET/NOT-MET call at fleet scale, which is why
	//     the verdict uses the fixed window.
	best60 := 0
	for i := range stamps {
		j := sort.Search(len(stamps), func(k int) bool {
			return stamps[k].After(stamps[i].Add(60 * time.Second))
		})
		if j-i > best60 {
			best60 = j - i
		}
	}

	fixed60 := 0
	if len(stamps) > 0 {
		winStart := stamps[0].Add(10 * time.Second)
		winEnd := winStart.Add(60 * time.Second)
		lo := sort.Search(len(stamps), func(k int) bool { return !stamps[k].Before(winStart) })
		hi := sort.Search(len(stamps), func(k int) bool { return !stamps[k].Before(winEnd) })
		fixed60 = hi - lo
	}

	perCluster := map[string]any{}
	for _, c := range clusters {
		perCluster[c.Name] = map[string]int64{
			"created": c.created.Load(),
			"ready":   c.ready.Load(),
		}
	}
	summary := map[string]any{
		"clusters":        len(clusters),
		"requested":       cfg.Count,
		"ready":           len(stamps),
		"createErrors":    errs,
		"aggregateRate":   cfg.Rate,
		"fixedWindow60s":  fixed60, // headline: fixed [firstReady+10s, +70s)
		"bestServer60s":   best60,  // reference: sliding max
		"targetPerMinute": 1000000,
		"met":             fixed60 >= 1000000,
		"wallSeconds":     time.Since(start).Seconds(),
	}
	out, _ := json.MarshalIndent(summary, "", "  ")
	log.Printf("FLEET RESULT: ready=%d fixed-60s-window(server clock)=%d (sliding-best=%d) met=%v",
		len(stamps), fixed60, best60, fixed60 >= 1000000)
	return os.WriteFile(filepath.Join(r.outDir, "summary.json"), append(out, '\n'), 0o644)
}
