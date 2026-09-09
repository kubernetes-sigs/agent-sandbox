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

// Wire types matching packages/sandboxd/spec/filesystem/v1/filesystem.yaml.
// Hand-written rather than generated: the repo carries no OpenAPI codegen
// toolchain and the surface is small; conformance is pinned by tests.

// FileEntry is one row of a DirectoryListing.
type FileEntry struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Type       string `json:"type"` // "file" | "directory"
	ModifiedAt string `json:"modified_at"`
	Mode       string `json:"mode,omitempty"` // octal, e.g. "0644"
}

// DirectoryListing is returned by GET /v1/files/{path} for directories.
type DirectoryListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// MetadataResponse is returned by GET /v1/metadata.
type MetadataResponse struct {
	Env map[string]string `json:"env"`
}

// APIError is the error body shared by all non-2xx responses.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
