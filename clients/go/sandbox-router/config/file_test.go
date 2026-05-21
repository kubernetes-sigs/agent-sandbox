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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestFileFromArgsAndEnv(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"env only", nil, map[string]string{EnvConfigFile: "/etc/c.yaml"}, "/etc/c.yaml"},
		{"--config FILE", []string{"--config", "/a.yaml"}, nil, "/a.yaml"},
		{"--config=FILE", []string{"--config=/b.yaml"}, nil, "/b.yaml"},
		{"-config FILE", []string{"-config", "/c.yaml"}, nil, "/c.yaml"},
		{"-config=FILE", []string{"-config=/d.yaml"}, nil, "/d.yaml"},
		{"CLI flag wins over env", []string{"--config", "/cli.yaml"}, map[string]string{EnvConfigFile: "/env.yaml"}, "/cli.yaml"},
		{"CLI flag (--config=value form) wins over env", []string{"--config=/cli.yaml"}, map[string]string{EnvConfigFile: "/env.yaml"}, "/cli.yaml"},
		{"none", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			got := FileFromArgsAndEnv(tc.args, func(k string) string { return env[k] })
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestLoadFromFile_AppliesValues(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	path := writeYAML(t, `
http-bind-address: ":18080"
cluster-domain: "prod.local"
proxy-timeout: "45s"
upstream-max-retries: 7
mtls-mode: "optional"
access-log: false
enable-tracing: true
`)
	if err := LoadFromFile(path, fs); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	if cfg.HTTPAddr != ":18080" {
		t.Errorf("HTTPAddr: %q", cfg.HTTPAddr)
	}
	if cfg.ClusterDomain != "prod.local" {
		t.Errorf("ClusterDomain: %q", cfg.ClusterDomain)
	}
	if cfg.ProxyTimeout != 45*time.Second {
		t.Errorf("ProxyTimeout: %s", cfg.ProxyTimeout)
	}
	if cfg.UpstreamMaxRetries != 7 {
		t.Errorf("UpstreamMaxRetries: %d", cfg.UpstreamMaxRetries)
	}
	if cfg.MTLSMode != MTLSOptional {
		t.Errorf("MTLSMode: %q", cfg.MTLSMode)
	}
	if cfg.AccessLog {
		t.Errorf("AccessLog should be false")
	}
	if !cfg.EnableTracing {
		t.Errorf("EnableTracing should be true")
	}
}

func TestLoadFromFile_CLIOverridesFile(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	path := writeYAML(t, `cluster-domain: "file.local"`)
	if err := LoadFromFile(path, fs); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	// Now simulate CLI flag parsing that overrides the file's value.
	if err := fs.Parse([]string{"--cluster-domain=cli.local"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ClusterDomain != "cli.local" {
		t.Fatalf("CLI should win over file; got %q", cfg.ClusterDomain)
	}
}

func TestLoadFromFile_UnknownKeyRejected(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	path := writeYAML(t, `bogus-key: "x"`)
	err := LoadFromFile(path, fs)
	if err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("expected unknown-key error, got: %v", err)
	}
}

func TestLoadFromFile_BadYAML(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	path := writeYAML(t, `cluster-domain: [not, a, scalar]`)
	err := LoadFromFile(path, fs)
	if err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Fatalf("expected composite-value error, got: %v", err)
	}
}

func TestLoadFromFile_MissingFile(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	cfg := Defaults()
	RegisterFlags(fs, &cfg, func(string) (string, bool) { return "", false })

	err := LoadFromFile("/no/such/file.yaml", fs)
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}
