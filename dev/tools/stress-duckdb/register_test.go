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

package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/",
			"https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/artifacts/stress-test",
		},
		{
			"https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/artifacts/stress-test",
			"https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/artifacts/stress-test",
		},
		{
			"gs://kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792",
			"https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/artifacts/stress-test",
		},
	}
	for _, tc := range cases {
		got := normalizeURL(tc.in)
		if got != tc.want {
			t.Errorf("normalizeURL(%q)=\n  %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestRunIDFromURL(t *testing.T) {
	url := "https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/1122/presubmit-agent-sandbox-benchmarks-kops-gcp/2075562098028449792/artifacts/stress-test"
	got, err := runIDFromURL(url)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2075562098028449792" {
		t.Errorf("runID=%q, want 2075562098028449792", got)
	}
}

func TestWriteNormalizedKeepsGzipBytes(t *testing.T) {
	dir := t.TempDir()
	plain := []byte(`{"n":1}` + "\n")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	gzBytes := buf.Bytes()

	a := artifactSpec{localBase: "metrics.jsonl", compress: true}
	if err := writeNormalized(dir, a, gzBytes); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "metrics.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, gzBytes) {
		t.Fatalf("expected gzip bytes preserved, got %d bytes", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "metrics.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("plain metrics.jsonl should not exist, err=%v", err)
	}
}

func TestWriteNormalizedGzipsPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := []byte(`{"n":1}` + "\n")
	a := artifactSpec{localBase: "metrics.jsonl", compress: true}
	if err := writeNormalized(dir, a, plain); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "metrics.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if !isGzip(got) {
		t.Fatal("expected gzip magic on written file")
	}
	gr, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(gr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("round-trip mismatch: %q", out.Bytes())
	}
}

func TestWriteNormalizedProwStrippedName(t *testing.T) {
	// Simulates Prow storing gzip bytes under metrics.jsonl (no .gz suffix).
	dir := t.TempDir()
	plain := []byte(`{"metric":"x"}` + "\n")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	a := artifactSpec{
		remoteNames: []string{"metrics.jsonl.gz", "metrics.jsonl"},
		localBase:   "metrics.jsonl",
		compress:    true,
	}
	if err := writeNormalized(dir, a, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "metrics.jsonl.gz")); err != nil {
		t.Fatal(err)
	}
}
