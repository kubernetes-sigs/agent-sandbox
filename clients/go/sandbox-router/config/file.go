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
	"fmt"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// EnvConfigFile is the environment variable consulted for a config file
// path when --config is not present on the command line.
const EnvConfigFile = "SANDBOX_ROUTER_CONFIG"

// FileFromArgsAndEnv returns the config file path from args (--config
// FILE, --config=FILE) or, if no --config flag is present, from the
// SANDBOX_ROUTER_CONFIG env var. Returns "" when neither source supplies
// a value.
//
// Precedence matches the overall config precedence (CLI > file > env >
// defaults): an explicit --config flag wins over the env var.
//
// This is invoked BEFORE flag.Parse because file values must be applied
// between flag registration and parsing so CLI flags take precedence.
func FileFromArgsAndEnv(args []string, lookupEnv func(string) string) string {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	for i := range args {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		}
	}
	if v := lookupEnv(EnvConfigFile); v != "" {
		return v
	}
	return ""
}

// LoadFromFile reads a YAML config from path and applies every key to the
// flag set fs via fs.Set. Keys must match registered flag names (kebab-case).
// Unknown keys, malformed YAML, and nested structures all return an error
// so operator typos surface at startup rather than silently no-op.
//
// Call sequence:
//
//	cfg := config.Defaults()
//	config.RegisterFlags(fs, &cfg, os.LookupEnv)  // env → defaults
//	if path != "" {
//	    config.LoadFromFile(path, fs)              // file overrides env
//	}
//	fs.Parse(args)                                 // CLI overrides file
//
// Precedence: CLI flags > file > env > built-in defaults.
func LoadFromFile(path string, fs *flag.FlagSet) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	// Strict-ish parse: top-level map of scalars. Nested values are rejected
	// since every flag is a scalar — supporting nested structures here would
	// quietly accept misconfigured files.
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config YAML: %w", err)
	}

	for key, value := range raw {
		if fs.Lookup(key) == nil {
			return fmt.Errorf("unknown config key %q in %s (must match a --flag name)", key, path)
		}
		s, err := scalarToString(value)
		if err != nil {
			return fmt.Errorf("config key %q: %w", key, err)
		}
		if err := fs.Set(key, s); err != nil {
			return fmt.Errorf("apply config key %q=%q: %w", key, s, err)
		}
	}
	return nil
}

// scalarToString converts a YAML scalar to the string form expected by
// flag.Value.Set. Composite values are rejected.
func scalarToString(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		// YAML numbers without a decimal still come back as float64 via
		// sigs.k8s.io/yaml. Render as int when whole to avoid surprising
		// flag.Set calls like "5.000000".
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case []any, map[string]any:
		return "", fmt.Errorf("must be a scalar; got composite value")
	default:
		return "", fmt.Errorf("unsupported scalar type %T", v)
	}
}
