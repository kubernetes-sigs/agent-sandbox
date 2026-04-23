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

// nolint:revive
package version

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestInfoDefault(t *testing.T) {
	info := Info{}

	testCases := []struct {
		name   string
		actual string
		expect string
	}{
		{"GitVersion", info.GitVersion, ""},
		{"GitSHA", info.GitSHA, ""},
		{"BuildDate", info.BuildDate, ""},
		{"GoVersion", info.GoVersion, ""},
		{"Compiler", info.Compiler, ""},
		{"Platform", info.Platform, ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.actual != tc.expect {
				t.Errorf("Expected %q, got %q", tc.expect, tc.actual)
			}
		})
	}
}

func TestInfoString(t *testing.T) {
	info := Info{
		GitVersion: "v1.0.0",
		GitSHA:     "abc123",
		BuildDate:  "2026-01-01T00:00:00Z",
		GoVersion:  "go1.22.0",
		Compiler:   "gc",
		Platform:   "linux/amd64",
	}

	s := info.String()
	testCases := []struct {
		name   string
		check  func(string) bool
		expect string
	}{
		{"ContainsTypeName", func(s string) bool { return strings.Contains(s, "version.Info") }, "version.Info"},
		{"ContainsGitVersion", func(s string) bool { return strings.Contains(s, "GitVersion:") }, "GitVersion:"},
		{"ContainsGoVersion", func(s string) bool { return strings.Contains(s, "GoVersion:") }, "GoVersion:"},
		{"ContainsGitSHA", func(s string) bool { return strings.Contains(s, "GitSHA:") }, "GitSHA:"},
		{"ContainsBuildDate", func(s string) bool { return strings.Contains(s, "BuildDate:") }, "BuildDate:"},
		{"ContainsCompiler", func(s string) bool { return strings.Contains(s, "Compiler:") }, "Compiler:"},
		{"ContainsPlatform", func(s string) bool { return strings.Contains(s, "Platform:") }, "Platform:"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.check(s) {
				t.Errorf("Expected %s in String output, got: %s", tc.expect, s)
			}
		})
	}
}

func TestInfoPrinted(t *testing.T) {
	platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)

	info := Get()

	testCases := []struct {
		name   string
		actual string
		expect string
	}{
		{"GitVersion", info.GitVersion, "unknown"},
		{"GitSHA", info.GitSHA, "unknown"},
		{"BuildDate", info.BuildDate, "unknown"},
		{"GoVersion", info.GoVersion, runtime.Version()},
		{"Compiler", info.Compiler, runtime.Compiler},
		{"Platform", info.Platform, platform},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.actual != tc.expect {
				t.Errorf("Expected %q, got %q", tc.expect, tc.actual)
			}
		})
	}
}
