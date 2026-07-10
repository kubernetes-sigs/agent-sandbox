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
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultJob     = "presubmit-agent-sandbox-benchmarks-kops-gcp"
	gcsBucket      = "kubernetes-ci-logs"
	gcsRepoPath    = "kubernetes-sigs_agent-sandbox"
	uiContainer    = "stress-duckdb-ui"
	uiFwdContainer = "stress-duckdb-ui-fwd"
)

type config struct {
	dataDir  string
	dbPath   string
	runsDir  string
	image    string
	uiPort   string
	viewsDir string
}

func loadConfig() (*config, error) {
	home := os.Getenv("STRESS_DUCKDB_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".cache", "agent-sandbox-stress")
	}
	image := os.Getenv("STRESS_DUCKDB_IMAGE")
	if image == "" {
		image = "duckdb/duckdb:latest"
	}
	port := os.Getenv("STRESS_DUCKDB_PORT")
	if port == "" {
		port = "4213"
	}

	pkg, err := packageDir()
	if err != nil {
		return nil, err
	}
	viewsDir := filepath.Join(pkg, "views")
	if v := os.Getenv("STRESS_DUCKDB_VIEWS"); v != "" {
		viewsDir = v
	}

	cfg := &config{
		dataDir:  home,
		dbPath:   filepath.Join(home, "stress.duckdb"),
		runsDir:  filepath.Join(home, "runs"),
		image:    image,
		uiPort:   port,
		viewsDir: viewsDir,
	}
	for _, d := range []string{cfg.dataDir, cfg.runsDir, filepath.Join(cfg.dataDir, ".duckdb")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return cfg, nil
}

func stressTestURL(pr int, job, buildID string) string {
	return fmt.Sprintf(
		"https://storage.googleapis.com/%s/pr-logs/pull/%s/%d/%s/%s/artifacts/stress-test",
		gcsBucket, gcsRepoPath, pr, job, buildID,
	)
}

func jobPrefix(pr int, job string) string {
	return fmt.Sprintf("pr-logs/pull/%s/%d/%s/", gcsRepoPath, pr, job)
}
