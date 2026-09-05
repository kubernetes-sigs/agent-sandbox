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
	"strings"
)

// InPodNamespaceFile is the well-known path where the kubelet projects the
// Pod's namespace via the downward API / service-account volume.
const InPodNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// MapFromArgsAndEnv returns the ConfigMap name from args
// (--config-configmap-name NAME, --config-configmap-name=NAME) or, if
// no flag is present, from the SANDBOX_ROUTER_CONFIG_CONFIGMAP_NAME env
// var. Returns "" when neither source supplies a value.
//
// Called BEFORE flag.Parse to detect whether ConfigMap loading was
// requested; the actual loading happens AFTER flag.Parse so ConfigMap
// values take the highest precedence (see the package doc).
func MapFromArgsAndEnv(args []string, lookupEnv func(string) string) string {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	for i := range args {
		a := args[i]
		switch {
		case a == "--config-configmap-name" || a == "-config-configmap-name":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--config-configmap-name="):
			return strings.TrimPrefix(a, "--config-configmap-name=")
		case strings.HasPrefix(a, "-config-configmap-name="):
			return strings.TrimPrefix(a, "-config-configmap-name=")
		}
	}
	if v := lookupEnv(EnvConfigMapName); v != "" {
		return v
	}
	return ""
}

// LoadFromConfigMapData applies every key in data to the flag set fs via
// fs.Set. Keys must match registered flag names (kebab-case). Unknown
// keys return an error so operator typos surface at startup.
//
// The caller is responsible for fetching the ConfigMap from the K8s API;
// this function is intentionally decoupled from the client to keep the
// config package free of k8s.io/client-go imports.
func LoadFromConfigMapData(data map[string]string, fs *flag.FlagSet) error {
	for key, value := range data {
		if strings.HasPrefix(key, "_") || strings.HasPrefix(key, ".") {
			continue
		}
		if fs.Lookup(key) == nil {
			return fmt.Errorf("unknown config key %q in ConfigMap (must match a --flag name)", key)
		}
		if err := fs.Set(key, value); err != nil {
			return fmt.Errorf("apply ConfigMap key %q=%q: %w", key, value, err)
		}
	}
	return nil
}

// InPodNamespace reads the namespace the router Pod is running in from
// the service-account namespace file. Returns ("", nil) when the file
// does not exist (not running in a Pod).
func InPodNamespace() (string, error) {
	data, err := os.ReadFile(InPodNamespaceFile)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read in-pod namespace: %w", err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("in-pod namespace file %s is empty", InPodNamespaceFile)
	}
	return ns, nil
}
