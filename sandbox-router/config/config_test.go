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
	"time"
)

// fakeEnv builds a LookupEnvFunc backed by a map. Missing keys return (_, false).
func fakeEnv(kv map[string]string) LookupEnvFunc {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestDefaultsAreValid(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Defaults() should validate, got: %v", err)
	}
}

func TestApplyEnvDefaults(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want func(*Config) bool
	}{
		{
			name: "cluster domain from env",
			env:  map[string]string{EnvClusterDomain: "prod.local"},
			want: func(c *Config) bool { return c.ClusterDomain == "prod.local" },
		},
		{
			name: "proxy timeout numeric seconds",
			env:  map[string]string{EnvProxyTimeout: "45"},
			want: func(c *Config) bool { return c.ProxyTimeout == 45*time.Second },
		},
		{
			name: "proxy timeout fractional seconds",
			env:  map[string]string{EnvProxyTimeout: "0.5"},
			want: func(c *Config) bool { return c.ProxyTimeout == 500*time.Millisecond },
		},
		{
			name: "proxy timeout invalid keeps default",
			env:  map[string]string{EnvProxyTimeout: "not-a-number"},
			want: func(c *Config) bool { return c.ProxyTimeout == 180*time.Second },
		},
		{
			name: "proxy timeout non-positive keeps default",
			env:  map[string]string{EnvProxyTimeout: "-1"},
			want: func(c *Config) bool { return c.ProxyTimeout == 180*time.Second },
		},
		{
			name: "empty env value keeps default",
			env:  map[string]string{EnvClusterDomain: ""},
			want: func(c *Config) bool { return c.ClusterDomain == "cluster.local" },
		},
		{
			name: "no env keeps defaults",
			env:  map[string]string{},
			want: func(c *Config) bool {
				return c.ClusterDomain == "cluster.local" && c.ProxyTimeout == 180*time.Second
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			applyEnvDefaults(&c, fakeEnv(tc.env))
			if !tc.want(&c) {
				t.Fatalf("env defaults check failed: %+v", c)
			}
		})
	}
}

func TestRegisterFlagsOverridesEnv(t *testing.T) {
	c := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{}) // silence usage
	RegisterFlags(fs, &c, fakeEnv(map[string]string{
		EnvClusterDomain: "from-env.local",
		EnvProxyTimeout:  "30",
	}))
	if err := fs.Parse([]string{
		"--cluster-domain=from-flag.local",
		"--proxy-timeout=15s",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ClusterDomain != "from-flag.local" {
		t.Errorf("flag should override env; got %q", c.ClusterDomain)
	}
	if c.ProxyTimeout != 15*time.Second {
		t.Errorf("flag should override env; got %s", c.ProxyTimeout)
	}
}

func TestAudiencesCSVFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "default empty", args: nil, want: nil},
		{name: "single", args: []string{"--authz-tokenreview-audiences=sandbox-router"}, want: []string{"sandbox-router"}},
		{name: "multi", args: []string{"--authz-tokenreview-audiences=a,b,c"}, want: []string{"a", "b", "c"}},
		{name: "trims whitespace", args: []string{"--authz-tokenreview-audiences=a, b , c"}, want: []string{"a", "b", "c"}},
		{name: "skips empty", args: []string{"--authz-tokenreview-audiences=a,,b"}, want: []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&strings.Builder{})
			RegisterFlags(fs, &c, func(string) (string, bool) { return "", false })
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(c.AuthzTokenReviewAudiences) != len(tc.want) {
				t.Fatalf("got %v want %v", c.AuthzTokenReviewAudiences, tc.want)
			}
			for i, v := range c.AuthzTokenReviewAudiences {
				if v != tc.want[i] {
					t.Fatalf("idx %d: got %q want %q", i, v, tc.want[i])
				}
			}
		})
	}
}

func TestApplyPostParseEnvDefaults(t *testing.T) {
	type expect struct {
		tracing bool
		metrics bool
	}
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want expect
	}{
		{
			name: "no env, no flag -> defaults stay off",
			args: nil,
			env:  nil,
			want: expect{tracing: false, metrics: false},
		},
		{
			name: "generic OTLP endpoint set -> both auto-enable",
			args: nil,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317"},
			want: expect{tracing: true, metrics: true},
		},
		{
			name: "signal-specific traces endpoint -> only tracing",
			args: nil,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://t:4317"},
			want: expect{tracing: true, metrics: false},
		},
		{
			name: "signal-specific metrics endpoint -> only metrics",
			args: nil,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://m:4317"},
			want: expect{tracing: false, metrics: true},
		},
		{
			name: "explicit --enable-tracing=false overrides env auto-enable",
			args: []string{"--enable-tracing=false"},
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317"},
			want: expect{tracing: false, metrics: true},
		},
		{
			name: "explicit --enable-otel-metrics=false overrides env",
			args: []string{"--enable-otel-metrics=false"},
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317"},
			want: expect{tracing: true, metrics: false},
		},
		{
			name: "explicit --enable-tracing=true with no env still on",
			args: []string{"--enable-tracing=true"},
			env:  nil,
			want: expect{tracing: true, metrics: false},
		},
		{
			name: "empty env value is treated as unset",
			args: nil,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ""},
			want: expect{tracing: false, metrics: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&strings.Builder{})
			RegisterFlags(fs, &c, func(string) (string, bool) { return "", false })
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			ApplyPostParseEnvDefaults(fs, &c, fakeEnv(tc.env))
			if c.EnableTracing != tc.want.tracing {
				t.Errorf("EnableTracing: got %v want %v", c.EnableTracing, tc.want.tracing)
			}
			if c.EnableOTelMetrics != tc.want.metrics {
				t.Errorf("EnableOTelMetrics: got %v want %v", c.EnableOTelMetrics, tc.want.metrics)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*Config)
		wantErr string // substring; empty means must succeed
	}{
		{
			name:    "defaults validate",
			mut:     func(_ *Config) {},
			wantErr: "",
		},
		{
			name:    "both addrs empty",
			mut:     func(c *Config) { c.HTTPAddr = ""; c.HTTPSAddr = "" },
			wantErr: "at least one of",
		},
		{
			name:    "invalid mtls mode",
			mut:     func(c *Config) { c.MTLSMode = "bogus" },
			wantErr: "invalid --mtls-mode",
		},
		{
			name: "https without cert",
			mut: func(c *Config) {
				c.HTTPSAddr = ":8443"
			},
			wantErr: "tls-cert-file",
		},
		{
			name: "mtls without https",
			mut: func(c *Config) {
				c.MTLSMode = MTLSRequired
			},
			wantErr: "requires --https-bind-address",
		},
		{
			name: "mtls without ca",
			mut: func(c *Config) {
				c.HTTPSAddr = ":8443"
				c.TLSCertFile = "/c"
				c.TLSKeyFile = "/k"
				c.MTLSMode = MTLSOptional
			},
			wantErr: "tls-client-ca-file",
		},
		{
			name:    "negative proxy timeout",
			mut:     func(c *Config) { c.ProxyTimeout = -1 * time.Second },
			wantErr: "proxy-timeout",
		},
		{
			name:    "zero response header timeout",
			mut:     func(c *Config) { c.ResponseHeaderTimeout = 0 },
			wantErr: "response-header-timeout",
		},
		{
			name:    "empty cluster domain",
			mut:     func(c *Config) { c.ClusterDomain = "" },
			wantErr: "cluster-domain",
		},
		{
			name:    "negative body limit",
			mut:     func(c *Config) { c.MaxRequestBodyBytes = -1 },
			wantErr: "max-request-body-bytes",
		},
		{
			name:    "negative upstream max retries",
			mut:     func(c *Config) { c.UpstreamMaxRetries = -1 },
			wantErr: "upstream-max-retries",
		},
		{
			name:    "negative upstream retry initial delay",
			mut:     func(c *Config) { c.UpstreamRetryInitialDelay = -1 * time.Second },
			wantErr: "upstream-retry-initial-delay",
		},
		{
			name:    "zero retries is valid (disables retries)",
			mut:     func(c *Config) { c.UpstreamMaxRetries = 0 },
			wantErr: "",
		},
		{
			name: "valid mtls configuration",
			mut: func(c *Config) {
				c.HTTPSAddr = ":8443"
				c.TLSCertFile = "/c"
				c.TLSKeyFile = "/k"
				c.TLSClientCAFile = "/ca"
				c.MTLSMode = MTLSRequired
			},
			wantErr: "",
		},
		{
			name:    "invalid authz mode",
			mut:     func(c *Config) { c.AuthzMode = "bogus" },
			wantErr: "invalid --authz-mode",
		},
		{
			name:    "zero tokenreview ttl",
			mut:     func(c *Config) { c.AuthzTokenReviewTTL = 0 },
			wantErr: "authz-tokenreview-ttl",
		},
		{
			name:    "negative tokenreview ttl",
			mut:     func(c *Config) { c.AuthzTokenReviewTTL = -1 * time.Second },
			wantErr: "authz-tokenreview-ttl",
		},
		{
			name:    "zero tokenreview cache size",
			mut:     func(c *Config) { c.AuthzTokenReviewCacheSize = 0 },
			wantErr: "authz-tokenreview-cache-size",
		},
		{
			name:    "negative tokenreview cache size",
			mut:     func(c *Config) { c.AuthzTokenReviewCacheSize = -1 },
			wantErr: "authz-tokenreview-cache-size",
		},
		{
			name: "valid tokenreview configuration",
			mut: func(c *Config) {
				c.AuthzMode = AuthzTokenReview
				c.AuthzTokenReviewRequireToken = true
				c.AuthzTokenReviewAudiences = []string{"sandbox-router"}
			},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mut(&c)
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected ok, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}
