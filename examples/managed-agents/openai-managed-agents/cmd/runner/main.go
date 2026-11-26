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

// Command runner is the in-sandbox daemon baked into the oma-runner
// image. It receives executor start/stop requests from the controller over
// HTTP and supervises `codex exec-server` (or fakeexec in tests), which
// connects outbound to the OpenAI Agents API and executes the session's
// tool calls inside the sandbox pod.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr/slogr"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/openai-managed-agents/internal/ports"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/openai-managed-agents/internal/runner"
)

func main() {
	var (
		addr        = flag.String("addr", fmt.Sprintf(":%d", ports.RunnerHTTP), "address the runner HTTP server listens on")
		execBin     = flag.String("exec-bin", envOr("RUNNER_EXEC_BIN", "codex"), "executor binary to supervise")
		execArgs    = flag.String("exec-args", "exec-server", "comma-separated args preceding --remote/--environment-id")
		stopGrace   = flag.Duration("stop-grace", 10*time.Second, "SIGTERM window before SIGKILL on executor stop")
		readyMarker = flag.String("ready-pattern", `(?i)^READY`, "stdout line pattern marking the executor as connected")
		remoteHosts = flag.String("allowed-remote-hosts", runner.DefaultAllowedRemoteHost, "comma-separated hosts an executor remote_url may use")
	)
	flag.Parse()

	log := slogr.NewLogr(slog.Default().Handler()).WithName("runner")

	cfg := runner.RunnerConfig{
		ListenAddr: *addr,
		ExecBin:    *execBin,
		Env:        nil,
		StopGrace:  *stopGrace,
		Log:        log,
	}
	for _, a := range strings.Split(*execArgs, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.ExecArgs = append(cfg.ExecArgs, a)
		}
	}
	for _, h := range strings.Split(*remoteHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			cfg.AllowedRemoteHosts = append(cfg.AllowedRemoteHosts, h)
		}
	}

	r := runner.NewRunner(cfg)
	if re, err := regexp.Compile(*readyMarker); err == nil {
		r.SetReadyPattern(re)
	} else {
		log.Error(err, "Invalid --ready-pattern; using the default")
	}

	if err := r.ListenAndServe(); err != nil {
		log.Error(err, "Runner exited")
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
