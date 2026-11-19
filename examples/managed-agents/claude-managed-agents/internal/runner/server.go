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

// Adapted from github.com/agent-substrate/claude-managed-agents (Apache-2.0,
// Copyright 2026 Google LLC).

// Package runner is the in-sandbox HTTP daemon. The controller notifies it
// of CMA work items (POST /work); it services them in-process with the
// environment worker while heartbeating the lease, and reports its state on
// /status and /idle so the controller can tell when a turn finished and when
// the sandbox went idle.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/envworker"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/ports"
)

// DefaultRunnerAddr matches containerPort in template.yaml and the
// controller's --runner-port default.
var DefaultRunnerAddr = fmt.Sprintf(":%d", ports.RunnerHTTP)

const (
	defaultRunTimeout = 10 * time.Minute

	// workerMaxIdle is how long the worker keeps the session open after the
	// agent's turn ends before exiting.
	workerMaxIdle = 1 * time.Second
)

// WorkRequest is the JSON payload accepted by POST /work.
type WorkRequest struct {
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id,omitempty"`
	// LastHeartbeat is the lease token the poller obtained when it acked the
	// work item and sent its first heartbeat. The worker quotes it as its
	// first expected_last_heartbeat, continuing that lease instead of
	// claiming a new one. Empty means the worker claims the lease itself.
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

// WorkResponse is the JSON payload returned by POST /work and /healthz.
type WorkResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// StatusResponse is the JSON payload returned by GET /status.
type StatusResponse struct {
	Running      bool   `json:"running"`
	SessionID    string `json:"session_id,omitempty"`
	WorkID       string `json:"work_id,omitempty"`
	LastActiveAt string `json:"last_active_at"`
}

// IdleStatus is the JSON payload returned by GET /idle.
type IdleStatus struct {
	IdleDuration string `json:"idle_duration"`
	LastActiveAt string `json:"last_active_at"`
}

// RunnerConfig configures the runner daemon.
type RunnerConfig struct {
	Addr string
}

// Defaults fills in default values for unset fields.
func (c *RunnerConfig) Defaults() {
	if c.Addr == "" {
		c.Addr = DefaultRunnerAddr
	}
}

// Runner serves the work protocol inside a sandbox pod.
type Runner struct {
	addr string

	// handleItem services one work item; tests replace it.
	handleItem func(ctx context.Context, req WorkRequest) error

	// workMu ensures that only one work item is serviced at a time.
	workMu sync.Mutex

	stateMu      sync.RWMutex
	running      bool
	sessionID    string
	workID       string
	lastActiveAt time.Time
	// completed remembers recently finished work IDs so a controller retry
	// that arrives after completion (its status polls failed) is answered
	// accepted instead of executing the item twice. Bounded by
	// completedMaxEntries.
	completed map[string]time.Time
}

// completedMaxEntries bounds the completed-work cache; the controller only
// ever retries the most recent item.
const completedMaxEntries = 32

// NewRunner creates a new Runner instance.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	cfg.Defaults()
	return &Runner{
		addr:         cfg.Addr,
		handleItem:   handleItem,
		lastActiveAt: time.Now(),
		completed:    make(map[string]time.Time),
	}, nil
}

// handleItem runs one work item with the in-process environment worker. The
// environment ID and key come from ANTHROPIC_ENVIRONMENT_ID and
// ANTHROPIC_ENVIRONMENT_KEY, which template.yaml injects from the
// cma-credentials Secret. The tools are the standard agent toolset rooted at
// the working directory (/workspace). The lease token and the environment
// key never reach the logs.
func handleItem(ctx context.Context, req WorkRequest) error {
	workdir, err := os.Getwd()
	if err != nil {
		workdir = "."
	}
	env := newToolContext(workdir)
	toolset := agenttoolset.BetaAgentToolset20260401(env)
	defer agenttoolset.CloseAll(toolset)

	maxIdle := workerMaxIdle
	worker := envworker.NewEnvironmentWorker(anthropic.NewClient(), envworker.EnvironmentWorkerOptions{
		Tools:        toolset,
		Workdir:      workdir,
		AllowedRoots: env.AllowedRoots,
		MaxIdle:      &maxIdle,
		Logger:       slog.Default(),
	})
	return worker.HandleItem(ctx, envworker.HandleItemOptions{
		WorkID:                req.WorkID,
		SessionID:             req.SessionID,
		ExpectedLastHeartbeat: req.LastHeartbeat,
	})
}

// newToolContext returns the agent toolset context. Tool calls execute inside
// the sandbox pod, which is the security boundary; the file tools are still
// confined to the workspace and /tmp so a prompt-injected agent cannot read
// the container's own credential mounts elsewhere.
func newToolContext(workdir string) *agenttoolset.AgentToolContext {
	return &agenttoolset.AgentToolContext{
		Workdir:      workdir,
		AllowedRoots: []string{"/workspace", "/tmp"},
	}
}

// Handler returns the http.Handler for the runner server.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", r.handleWork)
	mux.HandleFunc("/status", r.handleStatus)
	mux.HandleFunc("/idle", r.handleIdle)
	mux.HandleFunc("/healthz", r.handleHealthz)
	mux.HandleFunc("/readyz", r.handleHealthz)
	return mux
}

// handleWork validates the payload, marks the runner busy, answers 200
// immediately so proxies do not time out, and services the work item in a
// background goroutine.
func (r *Runner) handleWork(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		http.Error(rw, `{"error": "method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload WorkRequest
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&payload)
	}

	sessionID := strings.TrimSpace(payload.SessionID)
	if sessionID == "" {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Error:  "session_id is required",
		})
		return
	}
	payload.SessionID = sessionID
	payload.WorkID = strings.TrimSpace(payload.WorkID)
	if payload.WorkID == "" {
		payload.WorkID = sessionID
	}
	payload.LastHeartbeat = strings.TrimSpace(payload.LastHeartbeat)

	// Classify the notification under one lock so concurrent posts cannot
	// both start a run:
	//   - same work ID in flight or recently completed → accept, do not run
	//     (controller retry; the runner is idempotent per work ID)
	//   - different work ID while busy → 409, so the controller retries
	//     later instead of queueing a run that /status would hide
	r.stateMu.Lock()
	_, alreadyCompleted := r.completed[payload.WorkID]
	switch {
	case r.running && r.workID == payload.WorkID:
		r.stateMu.Unlock()
		r.acceptWork(rw, payload, "duplicate notification for work in flight")
		return
	case !r.running && alreadyCompleted:
		r.stateMu.Unlock()
		r.acceptWork(rw, payload, "retry after completion")
		return
	case r.running:
		r.stateMu.Unlock()
		slog.Info("Rejected work while another item is in flight",
			slog.String("session_id", payload.SessionID),
			slog.String("work_id", payload.WorkID),
		)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "busy",
			Error:  "another work item is in flight; retry later",
		})
		return
	}
	r.running = true
	r.sessionID = payload.SessionID
	r.workID = payload.WorkID
	r.lastActiveAt = time.Now()
	r.stateMu.Unlock()

	go func() {
		r.workMu.Lock()
		defer r.workMu.Unlock()

		// Only a successful run enters the completed cache: a failed run
		// must stay re-deliverable, so the same work ID runs again when
		// CMA hands it out once more. The cache write shares stateMu with
		// the running-flag reset to keep dedup checks atomic.
		succeeded := false
		defer func() {
			r.stateMu.Lock()
			r.running = false
			r.sessionID = ""
			r.workID = ""
			r.lastActiveAt = time.Now()
			if succeeded {
				r.rememberCompleted(payload.WorkID)
			}
			r.stateMu.Unlock()
		}()

		if err := r.StartWork(context.Background(), payload); err != nil {
			slog.Error("Background worker failed",
				slog.String("session_id", payload.SessionID),
				slog.Any("error", err),
			)
			return
		}
		succeeded = true
	}()

	r.acceptWork(rw, payload, "")
}

// acceptWork answers a POST /work with 200 accepted.
func (r *Runner) acceptWork(rw http.ResponseWriter, payload WorkRequest, note string) {
	if note != "" {
		slog.Info("Work notification accepted without a new execution",
			slog.String("session_id", payload.SessionID),
			slog.String("work_id", payload.WorkID),
			slog.String("reason", note),
		)
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{Status: "accepted"})
}

// rememberCompleted records a finished work ID, pruning the oldest entries
// past completedMaxEntries. Caller holds stateMu.
func (r *Runner) rememberCompleted(workID string) {
	now := time.Now()
	r.completed[workID] = now
	if len(r.completed) <= completedMaxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, v := range r.completed {
		if oldestKey == "" || v.Before(oldest) {
			oldestKey, oldest = k, v
		}
	}
	delete(r.completed, oldestKey)
}

// handleStatus reports whether a work item is in flight. The controller
// polls it to learn when a turn finished.
func (r *Runner) handleStatus(rw http.ResponseWriter, req *http.Request) {
	r.stateMu.RLock()
	resp := StatusResponse{
		Running:      r.running,
		SessionID:    r.sessionID,
		WorkID:       r.workID,
		LastActiveAt: r.lastActiveAt.UTC().Format(time.RFC3339),
	}
	r.stateMu.RUnlock()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(resp)
}

// handleIdle returns how long the runner has been idle.
func (r *Runner) handleIdle(rw http.ResponseWriter, req *http.Request) {
	r.stateMu.RLock()
	running := r.running
	lastActive := r.lastActiveAt
	r.stateMu.RUnlock()

	var idleDur time.Duration
	if !running {
		idleDur = time.Since(lastActive)
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(IdleStatus{
		IdleDuration: idleDur.String(),
		LastActiveAt: lastActive.UTC().Format(time.RFC3339),
	})
}

// handleHealthz returns OK status.
func (r *Runner) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{Status: "ok"})
}

// StartWork services one work item synchronously: it runs the session's tool
// calls in-process while heartbeating the lease, and returns when the session
// goes idle, is stopped, or the lease is lost.
func (r *Runner) StartWork(ctx context.Context, req WorkRequest) error {
	if strings.TrimSpace(req.SessionID) == "" {
		return errors.New("session_id is required")
	}

	slog.Info("Running worker",
		slog.String("session_id", req.SessionID),
		slog.String("work_id", req.WorkID),
		slog.Bool("lease_handoff", req.LastHeartbeat != ""),
	)
	err := r.handleItem(ctx, req)
	slog.Info("Worker finished",
		slog.String("session_id", req.SessionID),
		slog.String("work_id", req.WorkID),
		slog.Any("error", err),
	)
	return err
}

// Run starts the HTTP server with 10-minute timeouts.
func (r *Runner) Run() error {
	server := &http.Server{
		Addr:         r.addr,
		Handler:      r.Handler(),
		ReadTimeout:  defaultRunTimeout,
		WriteTimeout: defaultRunTimeout,
		IdleTimeout:  defaultRunTimeout,
	}
	return server.ListenAndServe()
}
