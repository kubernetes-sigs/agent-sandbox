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

// Package runner implements the in-sandbox HTTP daemon for the Bedrock
// Managed Agents bridge. It does not execute tool calls itself: it
// supervises the platform's executor (`codex exec-server`), which attaches
// outbound to BMA with AWS SigV4 and runs all shell/file/MCP tool calls
// driven by the hosted harness.
//
// The daemon's job is the executor lifecycle plus the status and idle
// endpoints the controller polls. The pod's AWS credentials are
// capabilities: they reach the executor through its environment and never
// appear in logs or responses.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/ports"
)

// DefaultBMAService is the SigV4 signing service of the BMA endpoint.
const DefaultBMAService = "bedrock-mantle"

// ExecRequest binds this sandbox's executor to one platform session. The
// executor's remote URL is built from the runner's own pod environment, so
// no endpoint crosses the wire.
type ExecRequest struct {
	SessionID     string `json:"session_id"`
	EnvironmentID string `json:"environment_id"`
	AttemptID     string `json:"attempt_id"`
}

// StatusResponse is the controller's view of the executor.
type StatusResponse struct {
	Alive         bool   `json:"alive"`
	Ready         bool   `json:"ready"`
	ExitCode      int    `json:"exit_code"`
	StderrTail    string `json:"stderr_tail,omitempty"`
	StopForced    bool   `json:"stop_forced"`
	SessionID     string `json:"session_id,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
	AttemptID     string `json:"attempt_id,omitempty"`
}

// RunnerConfig configures the daemon.
type RunnerConfig struct {
	// ListenAddr defaults to ":<ports.RunnerHTTP>".
	ListenAddr string
	// ExecBin is the executor binary ("codex" in production, fakeexec in tests).
	ExecBin string
	// ExecArgs prefixes the executor argv (e.g. ["exec-server"]).
	ExecArgs []string
	// Env is extra environment for the child (e.g. FAKEEXEC_MODE in tests).
	// The AWS credentials arrive via the pod environment and need no entry.
	Env []string
	// BMAEndpoint is the platform base URL the executor attaches to, e.g.
	// https://bedrock-mantle.us-east-1.api.aws. Required.
	BMAEndpoint string
	// BMAService is the SigV4 signing service passed to the executor.
	// Defaults to DefaultBMAService.
	BMAService string
	// AWSRegion is passed to the executor for SigV4 signing. Required.
	AWSRegion string
	// StopGrace is the SIGTERM window before SIGKILL. Default 10s.
	StopGrace time.Duration
	// Log receives operational logs; secrets never reach it.
	Log logr.Logger
}

func (c *RunnerConfig) defaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = fmt.Sprintf(":%d", ports.RunnerHTTP)
	}
	if c.StopGrace == 0 {
		c.StopGrace = 10 * time.Second
	}
	if c.BMAService == "" {
		c.BMAService = DefaultBMAService
	}
	if c.Log.GetSink() == nil {
		c.Log = logr.Discard()
	}
}

// Runner is the in-sandbox daemon.
type Runner struct {
	cfg RunnerConfig
	sup *Supervisor

	mu          sync.Mutex
	current     ExecRequest // binding of the running/last executor
	startGen    uint64      // bumps on every executor start
	stopForced  bool
	lastEventAt time.Time
}

// NewRunner builds a Runner; call Handler or ListenAndServe to serve.
func NewRunner(cfg RunnerConfig) *Runner {
	cfg.defaults()
	return &Runner{
		cfg:         cfg,
		sup:         NewSupervisor(cfg.ExecBin, cfg.Log),
		lastEventAt: time.Now(),
	}
}

// SetReadyPattern overrides the stdout pattern that marks the executor as
// connected (default: a line starting with READY).
func (r *Runner) SetReadyPattern(re *regexp.Regexp) {
	r.sup.SetReadyPattern(re)
}

// Handler routes the runner protocol.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /executor", r.handleExecutor)
	mux.HandleFunc("POST /executor/stop", r.handleStop)
	mux.HandleFunc("GET /status", r.handleStatus)
	mux.HandleFunc("GET /idle", r.handleIdle)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// ListenAndServe opens the idle placeholder listener (the Go SDK tunnel
// requires its second port to accept connections and holds them open; see
// ports.RunnerGRPCPlaceholder) and then serves HTTP until the process exits.
func (r *Runner) ListenAndServe() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", ports.RunnerGRPCPlaceholder))
	if err != nil {
		return fmt.Errorf("placeholder listener: %w", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open until the process exits: the SDK
			// tunnel keeps its gRPC leg established for the sandbox's life.
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	r.cfg.Log.Info("runner listening", "addr", r.cfg.ListenAddr)
	return http.ListenAndServe(r.cfg.ListenAddr, r.Handler())
}

func (r *Runner) handleExecutor(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": "reading body"})
		return
	}
	var er ExecRequest
	if err := json.Unmarshal(body, &er); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": "malformed JSON"})
		return
	}
	if er.SessionID == "" || er.EnvironmentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": "session_id and environment_id are required"})
		return
	}
	if err := r.validateConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "error": err.Error()})
		return
	}

	// The lock spans the state read, the Start call, and the binding write:
	// concurrent requests serialize here and the loser observes the running
	// executor (already_running or conflict) instead of double-starting it.
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.sup.State()
	bound := r.current

	if st.Alive {
		if bound.SessionID == er.SessionID && bound.EnvironmentID == er.EnvironmentID {
			r.lastEventAt = time.Now()
			writeJSON(w, http.StatusOK, map[string]string{"status": "already_running"})
			return
		}
		// A live executor bound to a different session is a claim/binding
		// bug, not a transient busy state: the controller abandons this
		// sandbox on 409 rather than waiting.
		r.cfg.Log.Error(nil, "executor binding conflict",
			"boundSession", bound.SessionID, "requestedSession", er.SessionID)
		writeJSON(w, http.StatusConflict, map[string]string{
			"status": "conflict",
			"error":  "executor is bound to a different session",
		})
		return
	}

	args := append(append([]string{}, r.cfg.ExecArgs...),
		"--remote", strings.TrimSuffix(r.cfg.BMAEndpoint, "/")+"/openai/v1",
		"--environment-id", er.EnvironmentID,
		"--remote-transport", "direct",
		"--aws-sigv4",
		"--aws-service", r.cfg.BMAService,
		"--aws-region", r.cfg.AWSRegion)
	if err := r.sup.Start(args, r.cfg.Env); err != nil {
		r.cfg.Log.Error(err, "executor start failed", "session", er.SessionID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	r.current = er
	r.startGen++
	r.stopForced = false
	r.lastEventAt = time.Now()
	r.cfg.Log.Info("executor started", "session", er.SessionID, "environment", er.EnvironmentID, "attempt", er.AttemptID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// validateConfig checks the executor's platform coordinates. The runner API
// is unauthenticated, so the endpoint must be an HTTPS api.aws host: the
// pod's AWS credentials must not be pointable elsewhere.
func (r *Runner) validateConfig() error {
	u, err := url.Parse(r.cfg.BMAEndpoint)
	if err != nil || u.Scheme != "https" {
		return errors.New("BMA_ENDPOINT must be an https:// URL")
	}
	if !strings.HasSuffix(u.Hostname(), ".api.aws") {
		return fmt.Errorf("BMA_ENDPOINT host %q is not an api.aws host", u.Hostname())
	}
	if r.cfg.AWSRegion == "" {
		return errors.New("AWS_REGION is required")
	}
	return nil
}

func (r *Runner) handleStop(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	gen := r.startGen
	r.mu.Unlock()
	if !r.sup.State().Alive {
		r.touch()
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_running"})
		return
	}
	forced := r.sup.Stop(r.cfg.StopGrace)
	r.mu.Lock()
	// Attribute the result to the executor this call stopped: a start that
	// raced the stop owns a newer generation and keeps its own state.
	if r.startGen == gen {
		r.stopForced = forced
	}
	r.lastEventAt = time.Now()
	r.mu.Unlock()
	r.cfg.Log.Info("executor stopped", "forced", forced)
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped", "forced": forced})
}

func (r *Runner) handleStatus(w http.ResponseWriter, _ *http.Request) {
	// One lock span so the process state and the binding describe the same
	// executor; the lock order (r.mu → supervisor.mu) matches handleExecutor.
	r.mu.Lock()
	st := r.sup.State()
	resp := StatusResponse{
		Alive:         st.Alive,
		Ready:         st.Ready,
		ExitCode:      st.ExitCode,
		StderrTail:    st.StderrTail,
		StopForced:    r.stopForced,
		SessionID:     r.current.SessionID,
		EnvironmentID: r.current.EnvironmentID,
		AttemptID:     r.current.AttemptID,
	}
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (r *Runner) handleIdle(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	last := r.lastEventAt
	r.mu.Unlock()
	// Idle time counts from the last controller interaction; a dead executor
	// reports idle since that same point so GC can proceed.
	writeJSON(w, http.StatusOK, map[string]any{
		"idle_seconds":  time.Since(last).Seconds(),
		"last_event_at": last.UTC().Format(time.RFC3339),
	})
}

func (r *Runner) touch() {
	r.mu.Lock()
	r.lastEventAt = time.Now()
	r.mu.Unlock()
}

// stopForTest terminates any running child so tests do not leak processes.
func (r *Runner) stopForTest() bool {
	return r.sup.Stop(r.cfg.StopGrace)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
