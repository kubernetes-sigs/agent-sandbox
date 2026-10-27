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
// The ConfigMap is volume-mounted at /etc/sandbox-config/; at startup the
// controller reads the directory and passes the key/value pairs to
// ApplyConfigMapData. To pick up changes, restart the controller
// Deployment.
//
// Resolution order: CLI flag > ConfigMap value > compiled default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
)

// tunableFlags is the explicit set of flags that may be overridden via
// the ConfigMap. Only flags in this allowlist are applied; everything
// else is ignored and reported as skipped so typos surface immediately.
// Do not modify after init.
var tunableFlags = map[string]bool{
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

// knownNonFlagKeys are ConfigMap keys consumed by other mechanisms
// (e.g. volume-mounted files) that should not be reported as skipped.
// Do not modify after init.
var knownNonFlagKeys = map[string]bool{
	"allowed-label-domains": true,
}

// IsIgnoredConfigKey reports whether a ConfigMap key is documentation-only
// (underscore or dot prefix) and should be silently skipped.
func IsIgnoredConfigKey(name string) bool {
	return strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// ApplyConfigMapData applies overrides from a ConfigMap data map to the
// given flag set. Only keys in the tunableFlags allowlist that match a
// registered flag name are applied. Explicit CLI flags always take
// precedence over ConfigMap values (CLI > ConfigMap > compiled defaults).
//
// Keys that are not in the allowlist, not documentation-only, and not in
// knownNonFlagKeys are returned in the skipped slice so the caller can
// log them (catches typos that would otherwise silently fall back to defaults).
// Keys that match a tunable flag but were already set on the CLI are
// returned in cliOverridden so the caller can log the precedence decision.
func ApplyConfigMapData(data map[string]string, fs *flag.FlagSet) (applied []Override, skipped, cliOverridden []string, _ error) {
	if len(data) == 0 {
		return nil, nil, nil, nil
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
		if knownNonFlagKeys[name] {
			continue
		}
		if !tunableFlags[name] {
			skipped = append(skipped, name)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			skipped = append(skipped, name)
			continue
		}
		if setOnCLI[name] {
			cliOverridden = append(cliOverridden, name)
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
		return applied, skipped, cliOverridden, errors.Join(errs...)
	}
	return applied, skipped, cliOverridden, nil
}

// Override records a single ConfigMap key that was applied to a flag.
type Override struct {
	Key   string
	Value string
}

// TunableFlagNames returns a sorted copy of the tunable flag name set.
func TunableFlagNames() []string {
	names := make([]string, 0, len(tunableFlags))
	for k := range tunableFlags {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
