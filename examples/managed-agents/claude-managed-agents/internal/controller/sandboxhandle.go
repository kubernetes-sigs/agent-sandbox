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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"

	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// statusPollInterval is how often WaitWorkFinished polls the runner.
const statusPollInterval = 2 * time.Second

// statusMaxConsecutiveErrors tolerates brief runner or port-forward hiccups
// (~30s at the poll interval) before the event fails and retries from
// EnsureReady, which re-establishes the tunnel.
const statusMaxConsecutiveErrors = 15

// HandleFactory builds session sandbox handles backed by the agent-sandbox
// Go SDK and an apiserver-brokered port-forward to the runner.
type HandleFactory struct {
	Client     *sdk.Client
	Forwarder  *PodPortForwarder
	Namespace  string
	WarmPool   string
	RunnerPort int
	HTTPClient *http.Client
	Logger     logr.Logger
}

// New returns the sessionSandbox implementation for one CMA session.
func (f *HandleFactory) New(sessionID string) sessionSandbox {
	return &sandboxHandle{
		f:         f,
		sessionID: sessionID,
		http:      f.HTTPClient,
		log:       f.Logger.WithValues("session_id", sessionID),
	}
}

// sandboxHandle owns one session's claim, port-forward, and runner calls.
// The SDK manages the SandboxClaim lifecycle; the handle adds the runner
// HTTP protocol on top.
type sandboxHandle struct {
	f         *HandleFactory
	sessionID string
	http      *http.Client
	log       logr.Logger

	mu      sync.Mutex
	sb      *sdk.Sandbox
	stopPF  func()
	baseURL string
}

// runnerWork mirrors the runner's WorkRequest JSON without importing the
// runner package, which would pull the agent toolset into the controller.
type runnerWork struct {
	SessionID     string `json:"session_id"`
	WorkID        string `json:"work_id,omitempty"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

// runnerStatus mirrors the runner's StatusResponse JSON.
type runnerStatus struct {
	Running bool `json:"running"`
}

// EnsureReady claims the sandbox on first use, resumes it if suspended, and
// (re-)establishes the port-forward. The forward is always recreated: after a
// resume the pod is new, and a stale tunnel can die silently.
func (h *sandboxHandle) EnsureReady(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.sb == nil {
		sb, err := h.f.Client.CreateSandbox(ctx, h.f.WarmPool, h.f.Namespace)
		if err != nil {
			return fmt.Errorf("claiming sandbox for session %s: %w", h.sessionID, err)
		}
		h.sb = sb
		h.log.Info("Claimed sandbox for session",
			"sandbox", sb.SandboxName(), "pod", sb.PodName())
	} else {
		suspended, err := h.sb.IsSuspended(ctx)
		if err != nil {
			return fmt.Errorf("checking sandbox suspension: %w", err)
		}
		if suspended {
			if err := h.sb.Resume(ctx); err != nil {
				return fmt.Errorf("resuming sandbox: %w", err)
			}
			h.log.Info("Resumed sandbox for session", "pod", h.sb.PodName())
		}
	}

	if h.stopPF != nil {
		h.stopPF()
		h.stopPF = nil
		h.baseURL = ""
	}
	baseURL, stop, err := h.f.Forwarder.Forward(ctx, h.f.Namespace, h.sb.PodName(), h.f.RunnerPort)
	if err != nil {
		return fmt.Errorf("port-forwarding to runner: %w", err)
	}
	h.baseURL, h.stopPF = baseURL, stop
	h.log.V(4).Info("Runner reachable", "base_url", baseURL)
	return nil
}

// PostWork notifies the runner of a work item and waits for acceptance.
func (h *sandboxHandle) PostWork(ctx context.Context, ev WorkEvent) error {
	h.mu.Lock()
	baseURL := h.baseURL
	h.mu.Unlock()
	if baseURL == "" {
		return errors.New("runner not connected; EnsureReady must run first")
	}

	body, err := json.Marshal(runnerWork{
		SessionID:     ev.SessionID,
		WorkID:        ev.WorkID,
		LastHeartbeat: ev.LastHeartbeat,
	})
	if err != nil {
		return fmt.Errorf("encoding work request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/work", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.http.Do(req)
	if err != nil {
		// Transport failure: the request may have reached the runner and the
		// response been lost, so delivery is uncertain.
		return newPostError(fmt.Errorf("POST /work: %w", err), true)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// 5xx leaves delivery uncertain (the runner may have accepted before
		// failing); other statuses are definite rejections.
		return newPostError(fmt.Errorf("POST /work: HTTP %d: %s", resp.StatusCode, respBody),
			resp.StatusCode >= 500)
	}
	if readErr != nil {
		// A 2xx whose body could not be read: the runner may well have
		// accepted before the response was truncated.
		return newPostError(fmt.Errorf("POST /work: reading acceptance: %w", readErr), true)
	}
	var wr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &wr); err != nil || wr.Status == "" {
		// A 2xx with a malformed or empty body carries no definite answer;
		// treat it like a truncated acceptance.
		return newPostError(fmt.Errorf("POST /work: malformed acceptance response %q", respBody), true)
	}
	if wr.Status != "accepted" {
		// The runner answered definitively without accepting.
		return newPostError(fmt.Errorf("POST /work: runner answered %q (%s)", wr.Status, wr.Error), false)
	}
	return nil
}

// WaitWorkFinished polls the runner's /status until the work item completes.
// The runner sets running=true before answering POST /work, so the first poll
// cannot race a not-yet-started work item.
func (h *sandboxHandle) WaitWorkFinished(ctx context.Context) error {
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(statusPollInterval):
		}

		running, err := h.fetchRunning(ctx)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= statusMaxConsecutiveErrors {
				return fmt.Errorf("runner unreachable: %w", err)
			}
			h.log.V(4).Info("Status poll failed, retrying", "error", err.Error())
			continue
		}
		consecutiveErrors = 0
		if !running {
			return nil
		}
	}
}

func (h *sandboxHandle) fetchRunning(ctx context.Context) (bool, error) {
	h.mu.Lock()
	baseURL := h.baseURL
	h.mu.Unlock()
	if baseURL == "" {
		return false, errors.New("runner not connected")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/status", nil)
	if err != nil {
		return false, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /status: HTTP %d", resp.StatusCode)
	}
	var st runnerStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return false, fmt.Errorf("decoding /status: %w", err)
	}
	return st.Running, nil
}

// Suspend tears down the port-forward and suspends the sandbox. Suspending
// terminates the pod; the workspace PVC survives and Resume brings up a new
// pod that reattaches it.
func (h *sandboxHandle) Suspend(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopPF != nil {
		h.stopPF()
		h.stopPF = nil
		h.baseURL = ""
	}
	if h.sb == nil {
		return nil
	}
	return h.sb.Suspend(ctx)
}

// Release deletes the claim, which deletes the sandbox and its PVCs. The
// handle is kept when Close fails so a retry can delete the claim again.
func (h *sandboxHandle) Release(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopPF != nil {
		h.stopPF()
		h.stopPF = nil
		h.baseURL = ""
	}
	if h.sb == nil {
		return nil
	}
	if err := h.sb.Close(ctx); err != nil {
		return err
	}
	h.sb = nil
	return nil
}
