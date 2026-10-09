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

// Package config applies controller tuning knobs from the
// agent-sandbox-config ConfigMap as flag overrides.
//
// At startup the controller fetches the ConfigMap from the API server
// (see fetchConfigMapData in cmd/agent-sandbox-controller) and passes its
// data map to ApplyConfigMapData. To pick up changes, restart the
// controller Deployment.
//
// Resolution order: CLI flag > ConfigMap value > compiled default.
package config

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// TunableFlags is the explicit set of flags that may be overridden via
// the ConfigMap. Only flags in this allowlist are applied; everything
// else is ignored and reported as skipped so typos surface immediately.
var TunableFlags = map[string]bool{
	"pprof-block-profile-rate":                         true,
	"pprof-mutex-profile-fraction":                     true,
	"kube-api-qps":                                     true,
	"kube-api-burst":                                   true,
	"api-connections":                                  true,
	"separate-watch-connection":                        true,
	"sandbox-concurrent-workers":                       true,
	"sandbox-claim-concurrent-workers":                 true,
	"sandbox-warm-pool-concurrent-workers":             true,
	"sandbox-template-concurrent-workers":              true,
	"sandbox-warm-pool-max-batch-size":                 true,
	"sandbox-warm-pool-replenish-delay":                true,
	"sandbox-warm-pool-max-refill-rate":                true,
	"sandbox-claim-warm-candidate-grace-period":        true,
	"sandbox-warm-pool-readiness-grace-period":         true,
	"sandbox-warm-pool-unschedulable-recheck-interval": true,
	"enable-warm-pool-eviction":                        true,
	"disable-sandbox-events":                           true,
	"disable-claim-events":                             true,
	"disable-claim-observability-annotations":          true,
	"sandbox-write-behind-window":                      true,
}

// KnownNonFlagKeys are ConfigMap keys consumed by other mechanisms
// (e.g. volume-mounted files) that should not be reported as skipped.
var KnownNonFlagKeys = map[string]bool{
	"allowed-label-domains": true,
}

// IsIgnoredConfigKey reports whether a ConfigMap key is documentation-only
// (underscore or dot prefix) and should be skipped for both flag overrides
// and change-detection hashing.
func IsIgnoredConfigKey(name string) bool {
	return strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// ApplyConfigMapData applies overrides from a ConfigMap data map to the
// given flag set. Only keys in the TunableFlags allowlist that match a
// registered flag name are applied. Explicit CLI flags always take
// precedence over ConfigMap values (CLI > ConfigMap > compiled defaults).
//
// Keys that are not in the allowlist, not documentation-only, and not in
// KnownNonFlagKeys are returned in the skipped slice so the caller can
// log them (catches typos that would otherwise silently fall back to defaults).
func ApplyConfigMapData(data map[string]string, fs *flag.FlagSet) (applied []Override, skipped []string, _ error) {
	if len(data) == 0 {
		return nil, nil, nil
	}

	// fs.Visit iterates only flags explicitly set on the command line,
	// letting us preserve CLI > ConfigMap precedence even though we run
	// after flag.Parse.
	setOnCLI := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setOnCLI[f.Name] = true })

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var errs []error
	for _, name := range keys {
		if IsIgnoredConfigKey(name) {
			continue
		}
		if KnownNonFlagKeys[name] {
			continue
		}
		if !TunableFlags[name] {
			skipped = append(skipped, name)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			skipped = append(skipped, name)
			continue
		}
		if setOnCLI[name] {
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
		return applied, skipped, fmt.Errorf("configmap parse errors: %v", errs)
	}
	return applied, skipped, nil
}

// Override records a single ConfigMap key that was applied to a flag.
type Override struct {
	Key   string
	Value string
}
