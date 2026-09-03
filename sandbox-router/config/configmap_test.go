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

package config

import (
	"flag"
	"strings"
	"testing"
)

func TestMapFromArgsAndEnv(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"env only", nil, map[string]string{EnvConfigMapName: "my-cm"}, "my-cm"},
		{"--config-configmap-name VALUE", []string{"--config-configmap-name", "cli-cm"}, nil, "cli-cm"},
		{"--config-configmap-name=VALUE", []string{"--config-configmap-name=eq-cm"}, nil, "eq-cm"},
		{"-config-configmap-name VALUE", []string{"-config-configmap-name", "short-cm"}, nil, "short-cm"},
		{"-config-configmap-name=VALUE", []string{"-config-configmap-name=short-eq"}, nil, "short-eq"},
		{"CLI wins over env", []string{"--config-configmap-name", "cli"}, map[string]string{EnvConfigMapName: "env"}, "cli"},
		{"none", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			got := MapFromArgsAndEnv(tc.args, func(k string) string {
				if env == nil {
					return ""
				}
				return env[k]
			})
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestLoadFromConfigMapData_AppliesValues(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	data := map[string]string{
		"cluster-domain":       "cm.local",
		"upstream-max-retries": "5",
		"access-log":           "false",
	}
	if err := LoadFromConfigMapData(data, fs); err != nil {
		t.Fatalf("LoadFromConfigMapData: %v", err)
	}

	if cfg.ClusterDomain != "cm.local" {
		t.Errorf("ClusterDomain: got %q want %q", cfg.ClusterDomain, "cm.local")
	}
	if cfg.UpstreamMaxRetries != 5 {
		t.Errorf("UpstreamMaxRetries: got %d want 5", cfg.UpstreamMaxRetries)
	}
	if cfg.AccessLog {
		t.Errorf("AccessLog: got true want false")
	}
}

func TestLoadFromConfigMapData_IgnoresDocumentationKeys(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	data := map[string]string{
		"_readme":        "This ConfigMap configures the sandbox-router.",
		".description":   "Another documentation key.",
		"cluster-domain": "cm.local",
	}
	if err := LoadFromConfigMapData(data, fs); err != nil {
		t.Fatalf("LoadFromConfigMapData should skip _/. keys, got: %v", err)
	}
	if cfg.ClusterDomain != "cm.local" {
		t.Errorf("ClusterDomain: got %q want %q", cfg.ClusterDomain, "cm.local")
	}
}

func TestLoadFromConfigMapData_UnknownKeyRejected(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	err := LoadFromConfigMapData(map[string]string{"bogus-key": "x"}, fs)
	if err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("expected unknown-key error, got: %v", err)
	}
}

func TestLoadFromConfigMapData_OverridesCLI(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	if err := fs.Parse([]string{"--cluster-domain=cli.local"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := LoadFromConfigMapData(map[string]string{"cluster-domain": "cm.local"}, fs); err != nil {
		t.Fatalf("LoadFromConfigMapData: %v", err)
	}
	if cfg.ClusterDomain != "cm.local" {
		t.Fatalf("ConfigMap should win over CLI; got %q", cfg.ClusterDomain)
	}
}

func TestLoadFromConfigMapData_OverridesFile(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	path := writeYAML(t, `cluster-domain: "file.local"`)
	if err := LoadFromFile(path, fs); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if err := LoadFromConfigMapData(map[string]string{"cluster-domain": "cm.local"}, fs); err != nil {
		t.Fatalf("LoadFromConfigMapData: %v", err)
	}

	if cfg.ClusterDomain != "cm.local" {
		t.Fatalf("ConfigMap should win over file; got %q", cfg.ClusterDomain)
	}
}

func TestLoadFromConfigMapData_EmptyMap(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	if err := LoadFromConfigMapData(map[string]string{}, fs); err != nil {
		t.Fatalf("empty map should succeed, got: %v", err)
	}
	if cfg.ClusterDomain != "cluster.local" {
		t.Errorf("defaults should be unchanged; ClusterDomain=%q", cfg.ClusterDomain)
	}
}
