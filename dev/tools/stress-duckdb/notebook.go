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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const stressAnalysisTitle = "Stress analysis"

// seedNotebooks inserts bundled DuckDB UI notebooks into ui.db.
// Must run while the UI is stopped: DuckDB takes an exclusive lock on ui.db.
//
// Physical layout: tables live in schema main of ui.db (queried as
// ui.main.notebook_* when the file is opened directly). The UI process exposes
// the same tables as _duckdb_ui.* via its storage extension — so inserts must
// target main.*, not a _duckdb_ui schema inside ui.db.
//
// Best-effort: a failure prints a hint but does not fail UI startup.
func seedNotebooks(ctx context.Context, cfg *config) {
	pkg, err := packageDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "notebook seed: %v\n", err)
		return
	}
	notebooksDir := filepath.Join(pkg, "notebooks")
	entries, err := os.ReadDir(notebooksDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notebook seed: %v\n", err)
		return
	}

	uiDir := filepath.Join(cfg.dataDir, ".duckdb", "extension_data", "ui")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "notebook seed: %v\n", err)
		return
	}

	if err := ensureUINotebookSchema(ctx, cfg, notebooksDir); err != nil {
		fmt.Fprintf(os.Stderr, "notebook seed: init schema: %v\n", err)
		fmt.Fprintf(os.Stderr, "  (open the UI once to create its catalog, then re-run; or paste from %s)\n", notebooksDir)
		return
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		title := stressAnalysisTitle
		if e.Name() != "stress-analysis.json" {
			title = strings.TrimSuffix(e.Name(), ".json")
		}
		if err := upsertNotebook(ctx, cfg, notebooksDir, e.Name(), title); err != nil {
			fmt.Fprintf(os.Stderr, "notebook seed %q: %v\n", title, err)
			fmt.Fprintf(os.Stderr, "  (JSON is at %s — import or paste into a new UI notebook)\n",
				filepath.Join(notebooksDir, e.Name()))
			continue
		}
		fmt.Printf("notebook ready: %q\n", title)
	}
}

func ensureUINotebookSchema(ctx context.Context, cfg *config, notebooksDir string) error {
	// Prefer the UI-created catalog. Only create tables if this is a fresh ui.db.
	// Schema matches what DuckDB UI / duckdb-nb-export reverse-engineered.
	const sql = `
CREATE TABLE IF NOT EXISTS main.notebooks (
  id UUID PRIMARY KEY,
  name VARCHAR NOT NULL,
  created TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS main.notebook_versions (
  notebook_id UUID NOT NULL,
  version INTEGER NOT NULL,
  title VARCHAR NOT NULL,
  json VARCHAR NOT NULL,
  created TIMESTAMP NOT NULL,
  expires TIMESTAMP,
  PRIMARY KEY (notebook_id, version)
);
CREATE TABLE IF NOT EXISTS main.current_notebook_id (
  id UUID NOT NULL
);
`
	return duckdbUIExec(ctx, cfg, notebooksDir, sql)
}

func upsertNotebook(ctx context.Context, cfg *config, notebooksDir, fileName, title string) error {
	escTitle := strings.ReplaceAll(title, "'", "''")
	containerJSON := "/notebooks/" + fileName
	// Match the community import recipe: name is notebook_<uuid>, title is separate.
	sql := fmt.Sprintf(`
INSTALL json; LOAD json;
CREATE OR REPLACE TEMP TABLE _nb_seed AS
  SELECT content AS json FROM read_text('%s');
SET variable notebook_content = (SELECT json FROM _nb_seed);
SET variable existing_id = (
  SELECT notebook_id FROM main.notebook_versions
  WHERE title = '%s' AND expires IS NULL
  ORDER BY version DESC LIMIT 1
);
SET variable current_timestamp = now();
BEGIN TRANSACTION;
  UPDATE main.notebook_versions
  SET json = getvariable('notebook_content'),
      created = getvariable('current_timestamp')
  WHERE notebook_id = getvariable('existing_id')
    AND expires IS NULL;

  SET variable new_id = uuid();
  INSERT INTO main.notebooks (id, name, created)
  SELECT getvariable('new_id'),
         'notebook_' || CAST(getvariable('new_id') AS VARCHAR),
         getvariable('current_timestamp')
  WHERE getvariable('existing_id') IS NULL;

  INSERT INTO main.notebook_versions (notebook_id, version, title, json, created, expires)
  SELECT getvariable('new_id'), 1, '%s', getvariable('notebook_content'), getvariable('current_timestamp'), NULL
  WHERE getvariable('existing_id') IS NULL;

  -- Point the UI at this notebook on next open.
  DELETE FROM main.current_notebook_id;
  INSERT INTO main.current_notebook_id (id)
  SELECT coalesce(getvariable('existing_id'), getvariable('new_id'));
COMMIT;
SELECT title, version, length(json) AS json_bytes,
       json_extract(json, '$.notebookSerializationFormat') AS fmt
FROM main.notebook_versions
WHERE title = '%s' AND expires IS NULL;
`, containerJSON, escTitle, escTitle, escTitle)

	out, err := duckdbUIExecOut(ctx, cfg, notebooksDir, sql)
	if err != nil {
		return err
	}
	if !strings.Contains(out, title) {
		return fmt.Errorf("insert reported ok but title %q not found in main.notebook_versions\n%s", title, out)
	}
	return nil
}

func duckdbUIExec(ctx context.Context, cfg *config, notebooksDir, sql string) error {
	_, err := duckdbUIExecOut(ctx, cfg, notebooksDir, sql)
	return err
}

func duckdbUIExecOut(ctx context.Context, cfg *config, notebooksDir, sql string) (string, error) {
	args := []string{
		"run", "--rm", "-i",
		"-v", cfg.dataDir + "/.duckdb:/root/.duckdb",
		"-v", notebooksDir + ":/notebooks:ro",
		cfg.image, "/duckdb",
		"/root/.duckdb/extension_data/ui/ui.db",
		"-c", sql,
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%w\n%s", err, s)
	}
	return s, nil
}
