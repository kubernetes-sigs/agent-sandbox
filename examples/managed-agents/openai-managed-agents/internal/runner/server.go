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

// Package runner implements the in-sandbox HTTP daemon for the OpenAI
// Agents API bridge. Unlike the Claude Managed Agents runner, it does not
// execute tool calls itself: it supervises the platform's executor
// (`codex exec-server`), which connects outbound to OpenAI and runs all
// shell/file/MCP tool calls driven by the hosted harness.
//
// The daemon's job is the executor lifecycle plus the status and idle
// endpoints the controller polls. The remote URL received in ExecRequest is
// a capability: it is passed to the child process and never logged or
// echoed in any response.
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
	"sync"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/openai-managed-agents/internal/ports"
)

// DefaultAllowedRemoteHost is the OpenAI Agents API self-hosted platform
// host the executor is expected to connect to.
const DefaultAllowedRemoteHost = "codex-cloud-environments.chatgpt.com"

// ExecRequest binds this sandbox's executor to one platform session.
type ExecRequest struct {
	SessionID     string `json:"session_id"`
	EnvironmentID string `json:"environment_id"`
	RemoteURL     string `json:"remote_url"`
	AttemptID     string `json:"attempt_id"`
}

// StatusResponse is the controller's view of the executor. RemoteURL is
// deliberately absent: responses must not carry the capability.
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
	// CODEX_API_KEY arrives via the pod environment and needs no entry here.
	Env []string
	// AllowedRemoteHosts are the hosts an executor remote_url may use.
	// Defaults to DefaultAllowedRemoteHost.
	AllowedRemoteHosts []string
	// StopGrace is the SIGTERM window before SIGKILL. Default 10s.
	StopGrace time.Duration
	// Log receives operational logs; secrets and remote URLs never reach it.
	Log logr.Logger
}

func (c *RunnerConfig) defaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = fmt.Sprintf(":%d", ports.RunnerHTTP)
	}
	if c.StopGrace == 0 {
		c.StopGrace = 10 * time.Second
	}
	if len(c.AllowedRemoteHosts) == 0 {
		c.AllowedRemoteHosts = []string{DefaultAllowedRemoteHost}
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
	if err := r.validateRemoteURL(er.RemoteURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": err.Error()})
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
		"--remote", er.RemoteURL, "--environment-id", er.EnvironmentID)
	if err := r.sup.Start(args, r.cfg.Env); err != nil {
		r.cfg.Log.Error(err, "executor start failed", "session", er.SessionID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	r.current = er
	r.stopForced = false
	r.lastEventAt = time.Now()
	r.cfg.Log.Info("executor started", "session", er.SessionID, "environment", er.EnvironmentID, "attempt", er.AttemptID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// validateRemoteURL confines the executor to the platform: the URL is a
// capability and this endpoint is unauthenticated, so an arbitrary
// destination must be rejected. Errors carry the host only, not the URL.
func (r *Runner) validateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" {
		return errors.New("remote_url must be a wss:// URL")
	}
	host := u.Hostname()
	for _, allowed := range r.cfg.AllowedRemoteHosts {
		if host == allowed {
			return nil
		}
	}
	return fmt.Errorf("remote_url host %q is not allowed", host)
}

func (r *Runner) handleStop(w http.ResponseWriter, _ *http.Request) {
	if !r.sup.State().Alive {
		r.touch()
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_running"})
		return
	}
	forced := r.sup.Stop(r.cfg.StopGrace)
	r.mu.Lock()
	r.stopForced = forced
	r.lastEventAt = time.Now()
	r.mu.Unlock()
	r.cfg.Log.Info("executor stopped", "forced", forced)
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped", "forced": forced})
}

func (r *Runner) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := r.sup.State()
	r.mu.Lock()
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
