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
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
)

func TestScrapeContainerd(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%t", unavailable), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/namespaces/kube-system/pods", "/api/v1/namespaces/agent-sandbox-system/pods":
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Query().Get("labelSelector") == "app.kubernetes.io/name=node-exporter" {
						fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[
							{"metadata":{"name":"exporter-a"},"spec":{"nodeName":"node-a","hostNetwork":true}},
							{"metadata":{"name":"exporter-pending"},"spec":{"hostNetwork":true}},
							{"metadata":{"name":"exporter-isolated"},"spec":{"nodeName":"node-b"}}
						]}`)
					} else {
						fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
					}
				case "/api/v1/nodes":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"apiVersion":"v1","kind":"NodeList","items":[]}`)
				case "/metrics":
					fmt.Fprint(w, "# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total 1\n")
				case "/api/v1/namespaces/kube-system/pods/http:exporter-a:9100/proxy/metrics",
					"/api/v1/namespaces/kube-system/pods/http:exporter-isolated:9100/proxy/metrics":
					fmt.Fprint(w, "# TYPE node_load1 gauge\nnode_load1 7\n")
				case "/api/v1/namespaces/kube-system/pods/http:exporter-a:1338/proxy/v1/metrics":
					if unavailable {
						http.Error(w, "connection refused", http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(w, `# TYPE containerd_cri_network_plugin_operations_duration_seconds_seconds histogram
containerd_cri_network_plugin_operations_duration_seconds_seconds_bucket{operation_type="set_up_pod",le="+Inf"} 4
containerd_cri_network_plugin_operations_duration_seconds_seconds_sum{operation_type="set_up_pod"} 2
containerd_cri_network_plugin_operations_duration_seconds_seconds_count{operation_type="set_up_pod"} 4
`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			path := filepath.Join(t.TempDir(), "metrics.jsonl.gz")
			scraper, err := newPromScraper(&rest.Config{Host: server.URL, QPS: -1}, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scraper.Close() })
			scraper.ScrapeAll(t.Context())
			if err := scraper.Close(); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			reader, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			decoder := json.NewDecoder(reader)
			var nodeSamples, containerdSamples int
			for {
				var sample metricSample
				if err := decoder.Decode(&sample); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch sample.Source {
				case "node":
					nodeSamples++
				case "containerd":
					containerdSamples++
					if sample.Instance != "node-a" {
						t.Errorf("containerd instance = %q, want node-a", sample.Instance)
					}
					if sample.Metric == "containerd_cri_network_plugin_operations_duration_seconds_seconds_sum" {
						if sample.Value != 2 || string(sample.Labels) != `{"operation_type":"set_up_pod"}` {
							t.Errorf("unexpected CNI duration sample: %+v", sample)
						}
					}
				}
			}
			if nodeSamples != 2 {
				t.Errorf("node samples = %d, want 2 even when containerd is unavailable", nodeSamples)
			}
			wantContainerd := 3
			if unavailable {
				wantContainerd = 0
			}
			if containerdSamples != wantContainerd {
				t.Errorf("containerd samples = %d, want %d", containerdSamples, wantContainerd)
			}
		})
	}
}
