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

package server

import (
	"net/http"
	"strings"
)

// sensitiveEnvMarkers guards /v1/metadata against accidentally exposing
// credentials even when they match the allowlist prefix: untrusted agent
// code can query this endpoint over loopback.
//
// This is defense in depth, not the primary control — a substring denylist
// can never enumerate every secret-shaped name. The primary control is the
// producer-side contract (spec): orchestrators and template authors must
// never inject credentials under the metadata prefix in the first place.
var sensitiveEnvMarkers = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CRED", "KEY",
	"AUTH", "BEARER", "PRIVATE", "CERT",
}

// handleMetadata serves GET /v1/metadata: orchestrator-injected, workload-
// scoped environment configuration. The response is computed once on first
// request (the daemon's environment is immutable after start) and cached.
func (s *RESTServer) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, MetadataResponse{Env: s.metadataEnv()})
}

// buildMetadataEnv filters the daemon environment down to the allowlisted
// prefix, withholding anything that looks like a credential.
func (s *RESTServer) buildMetadataEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range s.environ() {
		name, value, found := strings.Cut(kv, "=")
		if !found || !strings.HasPrefix(name, s.metadataEnvPrefix) {
			continue
		}
		if isSensitiveEnvName(name) {
			continue
		}
		env[name] = value
	}
	return env
}

// isSensitiveEnvName reports whether an environment variable name looks like
// it carries a credential. The spec forbids serving orchestrator credentials,
// API tokens, or cloud IAM keys on /v1/metadata.
func isSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range sensitiveEnvMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
