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

// Package metricsdocs extracts Prometheus metric definitions from Go source and
// renders them as a Markdown reference table.
//
// Extraction is fail-closed: a metric declaration it cannot fully
// resolve, and a declaration form it does not model at all, are both reported as
// errors instead of being skipped, so a new way of declaring a metric stops the
// build rather than quietly disappearing from the published reference. The
// signals it keys on are a Prometheus-qualified constructor call and a
// Prometheus Opts literal at the call site; a declaration that presents neither,
// such as an unqualified call handed options hoisted into a variable, can still
// pass unseen.
package metricsdocs

import (
	"fmt"
	"os"
	"path/filepath"
)

// familyType is the Prometheus metric type reported for a metric family.
type familyType string

// The Prometheus metric types that can appear in the generated reference.
const (
	typeCounter   familyType = "Counter"
	typeGauge     familyType = "Gauge"
	typeHistogram familyType = "Histogram"
	typeSummary   familyType = "Summary"
	typeUntyped   familyType = "Untyped"
)

// family is a single Prometheus metric family extracted from Go source.
type family struct {
	// Name is the fully qualified metric name, including any namespace and
	// subsystem prefix.
	Name string
	Type familyType
	Help string
	// Labels holds the variable label names in declaration order.
	Labels []string
	// ConstLabels holds the constant label names, sorted.
	ConstLabels []string
}

// Generate renders the metric definitions found in sourceDir to outputPath.
func Generate(sourceDir, outputPath string) error {
	families, err := extract(sourceDir)
	if err != nil {
		return err
	}
	// Either the wrong directory or an extractor that has stopped recognizing
	// the declarations. Writing an empty reference is worse than failing.
	if len(families) == 0 {
		return fmt.Errorf("no metric definitions found in %s", sourceDir)
	}
	return writeFile(outputPath, render(families))
}

// writeFile writes content through a temporary file in the destination
// directory so a failure cannot leave a partially rendered reference behind.
func writeFile(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	// os.CreateTemp uses 0600, which is too restrictive for a checked-in file.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting the mode of %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmp.Name(), path, err)
	}
	return nil
}
