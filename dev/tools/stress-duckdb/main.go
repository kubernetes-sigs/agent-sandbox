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

// stress-duckdb registers stress-test runs into a local DuckDB database and
// opens the DuckDB web UI (via docker; no local DuckDB install required).
//
// Usage:
//
//	go run ./stress-duckdb pr 1122
//	go run ./stress-duckdb register <url-or-dir>...
//	go run ./stress-duckdb ui [--listen ADDR]
//	go run ./stress-duckdb ui-stop
//	go run ./stress-duckdb sql "SELECT * FROM v_runs"
//
// Run from the dev/tools module directory (or: go run ./dev/tools/stress-duckdb
// after adding a go.work / using the tools module). "pr <N>" downloads the latest
// successful presubmit-agent-sandbox-benchmarks-kops-gcp run for that PR, rebuilds
// the database (including views from the views/ directory), and launches the UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	cmd, args := args[0], args[1:]
	switch cmd {
	case "pr":
		return cmdPR(ctx, cfg, args)
	case "register":
		return cmdRegister(ctx, cfg, args)
	case "ui":
		return cmdUI(ctx, cfg, args)
	case "ui-stop":
		return stopUI(cfg)
	case "sql":
		if len(args) == 0 {
			return errors.New("usage: stress-duckdb sql \"SELECT ...\"")
		}
		return duckdbExec(ctx, cfg, strings.Join(args, " "))
	case "help", "-h", "--help":
		fmt.Print(usage())
		return nil
	default:
		// Bare PR number: `stress-duckdb 1122`
		if _, err := strconv.Atoi(cmd); err == nil {
			return cmdPR(ctx, cfg, append([]string{cmd}, args...))
		}
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

func usage() string {
	return `stress-duckdb: explore agent-sandbox stress-test artifacts in DuckDB.

Commands:
  pr <N> [--job NAME] [--no-ui] [--listen ADDR]
      Download the latest successful stress run for PR N, rebuild the DB, open UI.
  register <url-or-dir>...
      Download/copy artifacts and rebuild the database (views from views/).
  ui [--listen ADDR]
      Serve the DuckDB web UI (default listen 127.0.0.1) and seed the
      "Stress analysis" starter notebook from notebooks/.
  ui-stop
      Stop a leftover UI container.
  sql "SELECT ..."
      Run a one-shot query.

Environment:
  STRESS_DUCKDB_HOME   data dir (default: ~/.cache/agent-sandbox-stress)
  STRESS_DUCKDB_IMAGE  duckdb docker image (default: duckdb/duckdb:latest)
  STRESS_DUCKDB_PORT   UI port (default: 4213)

Examples:
  go run ./stress-duckdb 1122                 # from dev/tools/
  go run ./stress-duckdb pr 1122 --no-ui
  go run ./stress-duckdb register ./my-artifacts/
`
}

func cmdPR(ctx context.Context, cfg *config, args []string) error {
	job := defaultJob
	noUI := false
	listen := "127.0.0.1"
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-ui":
			noUI = true
		case a == "--job" && i+1 < len(args):
			i++
			job = args[i]
		case strings.HasPrefix(a, "--job="):
			job = strings.TrimPrefix(a, "--job=")
		case a == "--listen" && i+1 < len(args):
			i++
			listen = args[i]
		case strings.HasPrefix(a, "--listen="):
			listen = strings.TrimPrefix(a, "--listen=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown pr option: %s\nusage: stress-duckdb pr <N> [--job NAME] [--no-ui] [--listen ADDR]", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		return errors.New("usage: stress-duckdb pr <N> [--job NAME] [--no-ui] [--listen ADDR]")
	}
	pr, err := strconv.Atoi(positional[0])
	if err != nil || pr <= 0 {
		return fmt.Errorf("invalid PR number %q", positional[0])
	}

	buildID, err := latestSuccessfulBuild(ctx, pr, job)
	if err != nil {
		return err
	}
	url := stressTestURL(pr, job, buildID)
	fmt.Printf("PR %d: latest successful %s build %s\n", pr, job, buildID)
	fmt.Printf("  %s\n", url)

	if err := registerOne(ctx, cfg, url); err != nil {
		return err
	}
	if err := rebuildDB(ctx, cfg); err != nil {
		return err
	}
	if noUI {
		return nil
	}
	return startUI(ctx, cfg, listen)
}

func cmdRegister(ctx context.Context, cfg *config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: stress-duckdb register <url-or-dir>...")
	}
	for _, src := range args {
		if err := registerOne(ctx, cfg, src); err != nil {
			return err
		}
	}
	return rebuildDB(ctx, cfg)
}

func cmdUI(ctx context.Context, cfg *config, args []string) error {
	listen := "127.0.0.1"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--listen" && i+1 < len(args):
			i++
			listen = args[i]
		case strings.HasPrefix(a, "--listen="):
			listen = strings.TrimPrefix(a, "--listen=")
		default:
			return fmt.Errorf("unknown ui option: %s", a)
		}
	}
	return startUI(ctx, cfg, listen)
}

// packageDir returns the directory containing this package's views/.
func packageDir() (string, error) {
	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
		dir := wd
		for i := 0; i < 8; i++ {
			candidates = append(candidates,
				filepath.Join(dir, "dev/tools/stress-duckdb"),
				filepath.Join(dir, "stress-duckdb"),
			)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if st, err := os.Stat(filepath.Join(c, "views")); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("cannot find views/ directory (set STRESS_DUCKDB_VIEWS or run from the repo)")
}
