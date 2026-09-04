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

package metricsdocs

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

// controllerMetricsDir is the package the checked-in reference is built from.
const controllerMetricsDir = "../metrics"

// TestExtractControllerMetrics is the acceptance baseline: it pins what the
// controller actually exposes, so adding, removing or relabelling a metric
// family has to be an explicit change here rather than a silent one.
func TestExtractControllerMetrics(t *testing.T) {
	want := []family{
		{
			Name: "agent_sandbox_build_info",
			Type: typeGauge,
			// Sorted, because they are declared in a map literal.
			ConstLabels: []string{"build_date", "compiler", "git_commit", "git_version", "go_version", "platform"},
		},
		{
			Name:   "agent_sandbox_claim_controller_startup_latency_ms",
			Type:   typeHistogram,
			Labels: []string{"launch_type", "sandbox_template"},
		},
		{
			Name:   "agent_sandbox_claim_creation_total",
			Type:   typeCounter,
			Labels: []string{"namespace", "sandbox_template", "launch_type", "warmpool_name", "pod_condition", "created_by"},
		},
		{
			Name:   "agent_sandbox_claim_startup_latency_ms",
			Type:   typeHistogram,
			Labels: []string{"launch_type", "sandbox_template"},
		},
		{
			Name:   "agent_sandbox_client_claim_startup_latency_ms",
			Type:   typeHistogram,
			Labels: []string{"launch_type", "sandbox_template"},
		},
		{
			Name:   "agent_sandbox_creation_latency_ms",
			Type:   typeHistogram,
			Labels: []string{"namespace", "launch_type", "sandbox_template"},
		},
		{
			// Declared as a bare descriptor, so its Gauge type comes from the
			// MustNewConstMetric call in sandbox_collector.go.
			Name:   "agent_sandboxes",
			Type:   typeGauge,
			Labels: []string{"namespace", "ready_condition", "expired", "launch_type", "sandbox_template", "owned_by", "created_by"},
		},
	}

	got, err := extract(controllerMetricsDir)
	require.NoError(t, err)
	// Help prose is pinned verbatim by the checked-in docs/metrics.md, which the
	// generated-file presubmit regenerates and diffs, and extraction refuses a
	// metric without it. Repeating it here would be a second hand-maintained copy
	// of the same text.
	require.Empty(t, cmp.Diff(want, got, cmpopts.EquateEmpty(), cmpopts.IgnoreFields(family{}, "Help")))
}

func TestGenerateRejectsPackageWithoutMetrics(t *testing.T) {
	source := sourceDir(t, map[string]string{
		"metrics.go": promSource(`var Registry = prometheus.NewRegistry()`),
	})

	err := Generate(source, filepath.Join(t.TempDir(), "metrics.md"))
	require.ErrorContains(t, err, "no metric definitions found")
}

// TestGenerateExtractsBeforeWriting guards the ordering rather than
// the atomic write: extraction must fully succeed before the output is touched,
// so the natural "open the file, write the preamble, then walk the AST" refactor
// cannot truncate a good reference on a bad run.
func TestGenerateExtractsBeforeWriting(t *testing.T) {
	source := sourceDir(t, map[string]string{
		"metrics.go": promSource(`var M = prometheus.NewCounter(prometheus.CounterOpts{Name: metricName, Help: "H."})`),
	})
	output := filepath.Join(t.TempDir(), "metrics.md")
	require.NoError(t, os.WriteFile(output, []byte("previous contents"), 0o600))

	require.Error(t, Generate(source, output))

	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, "previous contents", string(got))
}

// repoRoot is the module the controller binary is built from.
const repoRoot = "../.."

// TestPrometheusImportsStayInMetricsPackage backs the claim the
// generated reference makes about listing every family the controller exposes.
// A constructor added under controllers/ or extensions/ would register against
// the same controller-runtime registry, be scraped, and never be documented.
//
// The whole module is walked rather than a list of directories to cover, so a
// new top-level package is checked by default. Gitignored paths are excused by
// gitIgnored below; the rest are scope decisions: the sandbox-router is a
// separate binary with its own registry, nested modules are not built into the
// controller, and .git holds no Go source of ours.
func TestPrometheusImportsStayInMetricsPackage(t *testing.T) {
	// WalkDir builds paths from the relative root above, so the excused
	// directories need the same spelling.
	allowed := filepath.Join(repoRoot, "internal", "metrics")
	skipped := append(gitIgnored(t),
		filepath.Join(repoRoot, "sandbox-router"),
		filepath.Join(repoRoot, ".git"),
	)

	require.NoError(t, filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if slices.Contains(skipped, path) || isNestedModule(t, path) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == allowed {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution|parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			require.NotContains(t, imp.Path.Value, prometheusPath,
				"%s imports the Prometheus client; metrics belong in %s so they reach the generated reference", path, controllerMetricsDir)
		}
		return nil
	}))
}

// gitIgnored lists the paths .gitignore excludes, so this walk does not carry a
// second copy of that list. Several of them, bin and tmp among others, can hold
// scratch Go files that are not part of the controller and must not fail the
// test.
func gitIgnored(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Without this, a container-level failure such as safe.directory surfaces in
	// the blocking presubmit as a bare "exit status 128".
	require.NoError(t, err, "listing gitignored paths: %s", stderr.String())

	var paths []string
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if entry = strings.TrimSuffix(entry, "/"); entry != "" {
			paths = append(paths, filepath.Join(repoRoot, entry))
		}
	}
	return paths
}

// isNestedModule reports whether dir declares a module of its own, and so is
// built separately from the controller. The module root itself is not one.
func isNestedModule(t *testing.T, dir string) bool {
	t.Helper()
	if filepath.Clean(dir) == filepath.Clean(repoRoot) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	require.True(t, err == nil || os.IsNotExist(err), "checking %s for a nested module: %v", dir, err)
	return err == nil
}
