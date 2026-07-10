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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// artifactSpec describes one stress-test artifact we ingest.
//
// Large JSONL streams are always stored locally as .gz. Prow's uploader strips
// the .gz suffix from object names (WriterOptionsFromFileName), so we try both
// names remotely and normalize on download: keep gzip bytes as-is when the
// body is already compressed, otherwise gzip plaintext before writing.
type artifactSpec struct {
	// remoteNames are tried in order against the stress-test URL.
	remoteNames []string
	// localBase is the local filename without a compression suffix.
	localBase string
	// compress means the local file should be localBase+".gz".
	compress bool
}

var artifacts = []artifactSpec{
	{remoteNames: []string{"summary.json"}, localBase: "summary.json", compress: false},
	{remoteNames: []string{"sandboxes.jsonl"}, localBase: "sandboxes.jsonl", compress: true},
	{remoteNames: []string{"timeseries.jsonl"}, localBase: "timeseries.jsonl", compress: false},
	{remoteNames: []string{"metrics.jsonl.gz", "metrics.jsonl"}, localBase: "metrics.jsonl", compress: true},
	{remoteNames: []string{"watch.jsonl.gz", "watch.jsonl"}, localBase: "watch.jsonl", compress: true},
}

func normalizeURL(src string) string {
	src = strings.TrimRight(src, "/")
	src = strings.Replace(src, "https://gcsweb.k8s.io/gcs/", "https://storage.googleapis.com/", 1)
	src = strings.Replace(src, "gs://", "https://storage.googleapis.com/", 1)
	if !strings.HasSuffix(src, "/artifacts/stress-test") {
		src += "/artifacts/stress-test"
	}
	return src
}

func runIDFromURL(url string) (string, error) {
	const marker = "/artifacts/stress-test"
	if !strings.HasSuffix(url, marker) {
		return "", fmt.Errorf("cannot determine run id from %s", url)
	}
	base := strings.TrimSuffix(url, marker)
	id := base[strings.LastIndex(base, "/")+1:]
	if id == "" {
		return "", fmt.Errorf("cannot determine run id from %s", url)
	}
	return id, nil
}

func localPath(dir string, a artifactSpec) string {
	if a.compress {
		return filepath.Join(dir, a.localBase+".gz")
	}
	return filepath.Join(dir, a.localBase)
}

func registerOne(ctx context.Context, cfg *config, src string) error {
	var runID, dir string
	if st, err := os.Stat(src); err == nil && st.IsDir() {
		abs, err := filepath.Abs(src)
		if err != nil {
			return err
		}
		runID = filepath.Base(abs)
		dir = filepath.Join(cfg.runsDir, runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, a := range artifacts {
			if err := ingestLocalArtifact(abs, dir, a); err != nil {
				return err
			}
		}
	} else {
		url := normalizeURL(src)
		var err error
		runID, err = runIDFromURL(url)
		if err != nil {
			return err
		}
		dir = filepath.Join(cfg.runsDir, runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, a := range artifacts {
			if err := downloadArtifact(ctx, url, dir, a); err != nil {
				fmt.Printf("  (no %s)\n", a.localBase)
			}
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("run %s: no summary.json found, not registering", runID)
	}
	fmt.Printf("registered run %s in %s\n", runID, dir)
	return nil
}

func ingestLocalArtifact(srcDir, destDir string, a artifactSpec) error {
	var src string
	for _, name := range append([]string{a.localBase + ".gz", a.localBase}, a.remoteNames...) {
		candidate := filepath.Join(srcDir, name)
		if _, err := os.Stat(candidate); err == nil {
			src = candidate
			break
		}
	}
	if src == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeNormalized(destDir, a, data)
}

func downloadArtifact(ctx context.Context, baseURL, destDir string, a artifactSpec) error {
	var lastErr error
	for _, name := range a.remoteNames {
		data, err := downloadBytes(ctx, baseURL+"/"+name)
		if err != nil {
			lastErr = err
			continue
		}
		return writeNormalized(destDir, a, data)
	}
	return lastErr
}

// downloadClient disables transparent gzip decoding so Content-Encoding: gzip
// bodies stay compressed on the wire and can be written straight to *.gz.
func downloadClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			Proxy:              http.ProxyFromEnvironment,
			DisableCompression: true,
		},
	}
}

func downloadBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloadClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// writeNormalized writes an artifact so compressible streams always land as
// localBase+".gz": keep gzip bytes (magic 1f 8b) as-is, otherwise gzip plaintext.
func writeNormalized(dir string, a artifactSpec, data []byte) error {
	dest := localPath(dir, a)
	_ = os.Remove(filepath.Join(dir, a.localBase))
	_ = os.Remove(filepath.Join(dir, a.localBase+".gz"))

	if !a.compress {
		return os.WriteFile(dest, data, 0o644)
	}
	if isGzip(data) {
		return os.WriteFile(dest, data, 0o644)
	}
	return writeGzipFile(dest, data)
}

func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func writeGzipFile(path string, plain []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	if _, err := io.Copy(gw, bytes.NewReader(plain)); err != nil {
		_ = gw.Close()
		return err
	}
	return gw.Close()
}

func rebuildDB(ctx context.Context, cfg *config) error {
	sqlPath := filepath.Join(cfg.dataDir, "build.sql")
	sql, err := buildSQL(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sqlPath, []byte(sql), 0o644); err != nil {
		return err
	}
	if err := duckdbExec(ctx, cfg, ".read /data/build.sql"); err != nil {
		return fmt.Errorf("rebuild database: %w", err)
	}
	fmt.Println("database rebuilt")
	_ = duckdbExec(ctx, cfg, "SELECT run_id, k8s, worker_nodes, tp_ready_steady_per_s FROM v_runs")
	return nil
}

func buildSQL(cfg *config) (string, error) {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE runs_raw AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id, *
  FROM read_json_auto('/data/runs/*/summary.json', filename=true, union_by_name=true);
`)

	switch {
	case matchesAny(cfg.runsDir, "*/sandboxes.jsonl.gz"):
		b.WriteString(`CREATE OR REPLACE TABLE sandboxes AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id, *
  FROM read_json_auto('/data/runs/*/sandboxes.jsonl.gz', filename=true, union_by_name=true);
`)
	case matchesAny(cfg.runsDir, "*/sandboxes.jsonl"):
		b.WriteString(`CREATE OR REPLACE TABLE sandboxes AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id, *
  FROM read_json_auto('/data/runs/*/sandboxes.jsonl', filename=true, union_by_name=true);
`)
	}
	if matchesAny(cfg.runsDir, "*/timeseries.jsonl") {
		b.WriteString(`CREATE OR REPLACE TABLE timeseries AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id, *
  FROM read_json_auto('/data/runs/*/timeseries.jsonl', filename=true, union_by_name=true);
`)
	}
	if matchesAny(cfg.runsDir, "*/metrics.jsonl.gz") {
		b.WriteString(`CREATE OR REPLACE TABLE metrics AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id, * EXCLUDE (filename)
  FROM read_json_auto('/data/runs/*/metrics.jsonl.gz', filename=true, union_by_name=true);
`)
	}
	// Watch stream: force object to JSON so pods/nodes/events/sandboxes can
	// share one table despite different shapes.
	if matchesAny(cfg.runsDir, "*/watch.jsonl.gz") {
		b.WriteString(`CREATE OR REPLACE TABLE watch AS
  SELECT regexp_extract(filename, 'runs/([^/]+)/', 1) AS run_id,
         timestamp,
         resource,
         type,
         object
  FROM read_json('/data/runs/*/watch.jsonl.gz',
    filename=true,
    columns={
      'timestamp': 'TIMESTAMPTZ',
      'resource': 'VARCHAR',
      'type': 'VARCHAR',
      'object': 'JSON'
    },
    ignore_errors=true);
`)
	}

	views, err := loadViews(cfg.viewsDir)
	if err != nil {
		return "", err
	}
	for _, v := range views {
		b.WriteString("\n-- ")
		b.WriteString(v.name)
		b.WriteByte('\n')
		b.WriteString(v.sql)
		if !strings.HasSuffix(v.sql, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func matchesAny(root, pattern string) bool {
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	return err == nil && len(matches) > 0
}

type viewFile struct {
	name string
	sql  string
}

func loadViews(dir string) ([]viewFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading views dir %s: %w", dir, err)
	}
	var views []viewFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sql := strings.TrimSpace(string(body))
		if sql == "" {
			continue
		}
		views = append(views, viewFile{name: e.Name(), sql: sql})
	}
	if len(views) == 0 {
		return nil, fmt.Errorf("no .sql views found in %s", dir)
	}
	return views, nil
}
