// Copyright 2025 The Kubernetes Authors.
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

package config

import (
	"flag"
	"testing"
)

func TestApplyConfigMapData_NilData(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Int("sandbox-concurrent-workers", 100, "")

	applied, err := ApplyConfigMapData(nil, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("expected no overrides, got %v", applied)
	}
}

func TestApplyConfigMapData_OverridesDefault(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "200"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0].Key != "sandbox-concurrent-workers" {
		t.Errorf("expected 1 override for sandbox-concurrent-workers, got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200", workers)
	}
}

func TestApplyConfigMapData_ConfigMapWinsOverCLI(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "200"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{"-sandbox-concurrent-workers=150"})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override (ConfigMap wins over CLI), got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200 (ConfigMap wins)", workers)
	}
}

func TestApplyConfigMapData_NonTunableFlagIgnored(t *testing.T) {
	data := map[string]string{"leader-elect": "false"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var leaderElect bool
	fs.BoolVar(&leaderElect, "leader-elect", true, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("expected no overrides for non-tunable flag, got %v", applied)
	}
	if leaderElect != true {
		t.Errorf("leaderElect = %v, want true (non-tunable flag should not be overridden)", leaderElect)
	}
}

func TestApplyConfigMapData_BoolFlag(t *testing.T) {
	data := map[string]string{"enable-warm-pool-eviction": "false"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var eviction bool
	fs.BoolVar(&eviction, "enable-warm-pool-eviction", true, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override, got %v", applied)
	}
	if eviction != false {
		t.Errorf("eviction = %v, want false", eviction)
	}
}

func TestApplyConfigMapData_InvalidValue(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "not-a-number"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	_, err := ApplyConfigMapData(data, fs)
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
	if workers != 100 {
		t.Errorf("workers = %d, want 100 (unchanged)", workers)
	}
}

func TestApplyConfigMapData_UnknownKeysIgnored(t *testing.T) {
	data := map[string]string{
		"unknown-key":                "value",
		"sandbox-concurrent-workers": "200",
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override (unknown keys ignored), got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200", workers)
	}
}

func TestApplyConfigMapData_WhitespaceHandling(t *testing.T) {
	data := map[string]string{"sandbox-concurrent-workers": "  200  \n"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override, got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200", workers)
	}
}

func TestApplyConfigMapData_FloatFlag(t *testing.T) {
	data := map[string]string{"kube-api-qps": "50.5"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var qps float64
	fs.Float64Var(&qps, "kube-api-qps", -1.0, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override, got %v", applied)
	}
	if qps != 50.5 {
		t.Errorf("qps = %f, want 50.5", qps)
	}
}

func TestApplyConfigMapData_MultipleOverrides(t *testing.T) {
	data := map[string]string{
		"sandbox-concurrent-workers":       "200",
		"sandbox-warm-pool-max-batch-size": "500",
		"enable-warm-pool-eviction":        "false",
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers, batchSize int
	var eviction bool
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	fs.IntVar(&batchSize, "sandbox-warm-pool-max-batch-size", 300, "")
	fs.BoolVar(&eviction, "enable-warm-pool-eviction", true, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 3 {
		t.Errorf("expected 3 overrides, got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200", workers)
	}
	if batchSize != 500 {
		t.Errorf("batchSize = %d, want 500", batchSize)
	}
	if eviction != false {
		t.Errorf("eviction = %v, want false", eviction)
	}
}

func TestApplyConfigMapData_EmptyData(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Int("sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(map[string]string{}, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("expected no overrides for empty data, got %v", applied)
	}
}

func TestApplyConfigMapData_UnderscorePrefixSkipped(t *testing.T) {
	data := map[string]string{
		"_readme":                    "some documentation text",
		"sandbox-concurrent-workers": "200",
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var workers int
	fs.IntVar(&workers, "sandbox-concurrent-workers", 100, "")
	_ = fs.Parse([]string{})

	applied, err := ApplyConfigMapData(data, fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("expected 1 override (_readme skipped), got %v", applied)
	}
	if workers != 200 {
		t.Errorf("workers = %d, want 200", workers)
	}
}

func TestApplyConfigMapData_ZapFlagsNonTunable(t *testing.T) {
	for _, name := range []string{
		"zap-devel",
		"zap-encoder",
		"zap-log-level",
		"zap-stacktrace-level",
		"zap-time-encoding",
	} {
		t.Run(name, func(t *testing.T) {
			data := map[string]string{name: "debug"}

			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			var value string
			fs.StringVar(&value, name, "info", "")
			_ = fs.Parse([]string{})

			applied, err := ApplyConfigMapData(data, fs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(applied) != 0 {
				t.Errorf("expected no overrides for %s, got %v", name, applied)
			}
			if value != "info" {
				t.Errorf("%s = %q, want info (zap flags are non-tunable)", name, value)
			}
		})
	}
}
