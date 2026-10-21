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

package benchmarkskopsgcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestCgroupStatCollection(t *testing.T) {
	manifest, err := os.ReadFile("node-exporter.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(manifest, &ds); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == "cgroup-stat" {
			if len(container.Command) != 3 || container.Command[0] != "/bin/sh" || container.Command[1] != "-c" {
				t.Fatalf("unexpected collector command: %v", container.Command)
			}
			script = container.Command[2]
		}
	}
	if script == "" {
		t.Fatal("cgroup-stat collector missing")
	}
	// Run the deployed script against fixtures, stopping at its first sleep.
	// Capture the published file before the exit trap cleans it up.
	script = strings.NewReplacer(
		"/host/sys/fs/cgroup", `"$TEST_CGROUP_DIR"`,
		"/textfile", `"$TEST_TEXTFILE_DIR"`,
	).Replace(script)
	script = "sleep() { cp \"$TEST_TEXTFILE_DIR/cgroup_stat.prom\" \"$TEST_SNAPSHOT\"; exit 0; }\n" + script

	for _, tt := range []struct {
		name      string
		input     string
		fault     string
		want      map[string]float64
		wantError bool
	}{
		{
			name:  "older kernel",
			input: "nr_descendants 12\nnr_dying_descendants 3\n",
			want: map[string]float64{
				"node_cgroup_stat_nr_descendants":       12,
				"node_cgroup_stat_nr_dying_descendants": 3,
				"node_cgroup_stat_collection_success":   1,
			},
		},
		{
			name:  "subsystem counts",
			input: "nr_descendants 12\nnr_subsys_memory 13\nnr_dying_descendants 3\nnr_dying_subsys_memory 7\n",
			want: map[string]float64{
				"node_cgroup_stat_nr_descendants":         12,
				"node_cgroup_stat_nr_subsys_memory":       13,
				"node_cgroup_stat_nr_dying_descendants":   3,
				"node_cgroup_stat_nr_dying_subsys_memory": 7,
				"node_cgroup_stat_collection_success":     1,
			},
		},
		{
			name: "missing source clears previous counts",
			want: map[string]float64{"node_cgroup_stat_collection_success": 0},
		},
		{
			name:  "read failure discards partial output and previous counts",
			input: "nr_descendants 12\n",
			fault: "awk() { printf 'partial output'; return 1; }\n",
			want:  map[string]float64{"node_cgroup_stat_collection_success": 0},
		},
		{
			name:      "publication failure removes previous counts",
			input:     "nr_descendants 12\n",
			fault:     "mv() { return 1; }\n",
			wantError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.input != "" {
				if err := os.WriteFile(filepath.Join(dir, "cgroup.stat"), []byte(tt.input), 0644); err != nil {
					t.Fatal(err)
				}
			}
			outputPath := filepath.Join(dir, "cgroup_stat.prom")
			if err := os.WriteFile(outputPath, []byte("# TYPE node_cgroup_stat_nr_descendants gauge\nnode_cgroup_stat_nr_descendants 999\n"), 0644); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(dir, "snapshot.prom")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", tt.fault+script)
			cmd.Env = append(os.Environ(), "TEST_CGROUP_DIR="+dir, "TEST_TEXTFILE_DIR="+dir, "TEST_SNAPSHOT="+snapshot)
			if output, err := cmd.CombinedOutput(); (err != nil) != tt.wantError {
				t.Fatalf("collector error = %v, want error %v: %s", err, tt.wantError, output)
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			for _, path := range []string{outputPath, outputPath + ".tmp"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("collector left %s after exit: %v", path, err)
				}
			}
			if tt.wantError {
				return
			}
			data, err := os.ReadFile(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			parser := expfmt.NewTextParser(model.UTF8Validation)
			families, err := parser.TextToMetricFamilies(strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("invalid Prometheus output: %v\n%s", err, data)
			}
			got := make(map[string]float64)
			for name, family := range families {
				if family.GetType() != dto.MetricType_GAUGE || len(family.Metric) != 1 {
					t.Fatalf("expected one gauge for %s: %v", name, family)
				}
				got[name] = family.Metric[0].GetGauge().GetValue()
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("metrics mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
