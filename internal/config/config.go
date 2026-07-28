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

// Package config reads controller tuning knobs from a mounted ConfigMap
// directory and applies them as flag overrides. The ConfigMap is expected
// to be mounted at a well-known path (default /etc/sandbox-config) and
// each key is a file whose contents hold the value.
//
// Resolution order: ConfigMap value > CLI flag > compiled default.
// Changes to the ConfigMap require a controller restart to take effect.
package config

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// NonTunableFlags is the set of structural/identity flags that must NOT
// be overridden via the ConfigMap. Everything else is tunable.
var NonTunableFlags = map[string]bool{
	"version":                  true,
	"webhook-port":             true,
	"webhook-cert-dir":         true,
	"webhook-cert-name":        true,
	"webhook-key-name":         true,
	"webhook-service-name":     true,
	"webhook-namespace":        true,
	"manage-webhook-certs":     true,
	"enable-webhook":           true,
	"cluster-domain":           true,
	"metrics-bind-address":     true,
	"health-probe-bind-address": true,
	"leader-elect":             true,
	"leader-election-namespace": true,
	"extensions":               true,
	"enable-tracing":           true,
	"enable-pprof":             true,
	"enable-pprof-debug":       true,
	"cache-label-selectors":    true,
}

// ApplyConfigMapData applies overrides from a ConfigMap data map to the
// given flag set. Keys matching a registered flag name (that are not in
// the NonTunableFlags denylist) are applied. ConfigMap values always
// take precedence over CLI flags.
func ApplyConfigMapData(data map[string]string, fs *flag.FlagSet) (applied []Override, _ error) {
	if len(data) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var errs []error
	for _, name := range keys {
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if NonTunableFlags[name] {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}

		raw := strings.TrimSpace(data[name])
		prev := f.Value.String()
		if err := f.Value.Set(raw); err != nil {
			_ = f.Value.Set(prev)
			errs = append(errs, fmt.Errorf("configmap key %q: %w", name, err))
			continue
		}

		applied = append(applied, Override{Key: name, Value: raw})
	}

	if len(errs) > 0 {
		return applied, fmt.Errorf("configmap parse errors: %v", errs)
	}
	return applied, nil
}

// Override records a single ConfigMap key that was applied to a flag.
type Override struct {
	Key   string
	Value string
}

