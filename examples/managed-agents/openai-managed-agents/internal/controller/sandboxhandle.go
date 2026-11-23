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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

const (
	// statusPollInterval is how often the wait loops poll the runner.
	statusPollInterval = 2 * time.Second

	// statusMaxConsecutiveErrors tolerates brief runner or port-forward
	// hiccups (~30s at the poll interval) before a wait fails and the event
	// retries from EnsureReady, which re-establishes the tunnel.
	statusMaxConsecutiveErrors = 15

	// turnPollInterval is how often WaitTurnSettled queries the platform.
	turnPollInterval = 5 * time.Second

	// turnMaxConsecutiveErrors bounds platform hiccups during turn polling.
	// Fewer than the runner's tolerance: the platform API is the source of
	// truth and a longer blind wait risks missing a settled turn by minutes.
	turnMaxConsecutiveErrors = 3

	// stopExecutorTimeout bounds the graceful-stop call before suspension.
	stopExecutorTimeout = 15 * time.Second

	// operatingModeReadyTimeout bounds the wait for a Sandbox to report a
	// condition observed at the generation an operatingMode patch produced.
	operatingModeReadyTimeout = 3 * time.Minute
)

// HandleFactory builds session sandbox handles backed by the agent-sandbox
// Go SDK and an apiserver-brokered port-forward to the runner.
type HandleFactory struct {
	Client *sdk.Client
	// K8s gives the handle direct Sandbox CR access for the cases the SDK
	// client cannot serve: resuming a suspended sandbox before adopting its
	// claim, and suspending one whose handle was never opened. Nil disables
	// both (tests).
	K8s        *sdk.K8sHelper
	Forwarder  *PodPortForwarder
	Namespace  string
	WarmPool   string
	RunnerPort int
	HTTPClient *http.Client
	// API answers turn-state queries for WaitTurnSettled.
	API    AgentAPI
	Logger logr.Logger
}

// New returns the sessionSandbox implementation for one Agents API session.
func (f *HandleFactory) New(sessionID string) sessionSandbox {
	return &sandboxHandle{
		f:            f,
		sessionID:    sessionID,
		claimName:    ClaimNameForSession(sessionID),
		http:         f.HTTPClient,
		log:          f.Logger.WithValues("session_id", sessionID),
		pollInterval: statusPollInterval,
		turnInterval: turnPollInterval,
	}
}

// ClaimNameForSession derives a deterministic SandboxClaim name from the
// session ID. The SDK adopts an existing claim of the same name, so a
// controller restart re-attaches to the sandbox a session already holds
// instead of leaking it — the bridge keeps no local state. The hash keeps
// the name within DNS-1123 limits for arbitrary session IDs.
func ClaimNameForSession(sessionID string) string {
	sum := sha1.Sum([]byte(sessionID))
	return "oai-" + hex.EncodeToString(sum[:])[:32]
}

// sandboxHandle owns one session's claim, port-forward, and runner calls.
// The SDK manages the SandboxClaim lifecycle; the handle adds the runner
// HTTP protocol and the platform turn-state queries on top.
type sandboxHandle struct {
	f         *HandleFactory
	sessionID string
	claimName string
	http      *http.Client
	log       logr.Logger

	// pollInterval and turnInterval mirror the package constants; tests
	// shrink them.
	pollInterval time.Duration
	turnInterval time.Duration

	mu      sync.Mutex
	sb      *sdk.Sandbox
	stopPF  func()
	baseURL string
}

// runnerExec mirrors the runner's ExecRequest JSON without importing the
// runner package.
type runnerExec struct {
	SessionID     string `json:"session_id"`
	EnvironmentID string `json:"environment_id"`
	RemoteURL     string `json:"remote_url"`
	AttemptID     string `json:"attempt_id"`
}

// runnerStatus mirrors the runner's StatusResponse JSON.
type runnerStatus struct {
	Alive      bool   `json:"alive"`
	Ready      bool   `json:"ready"`
	ExitCode   int    `json:"exit_code"`
	StderrTail string `json:"stderr_tail"`
	SessionID  string `json:"session_id"`
}

// EnsureReady claims the sandbox on first use (adopting the session's
// deterministic claim after a controller restart), resumes it if suspended,
// and (re-)establishes the port-forward. The forward is always recreated:
// after a resume the pod is new, and a stale tunnel can die silently.
func (h *sandboxHandle) EnsureReady(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.sb == nil {
		// A restarted controller can adopt a claim whose sandbox is
		// suspended; the SDK's readiness wait cannot succeed until it is
		// resumed, so flip it to Running first.
		if err := h.resumeIfSuspended(ctx); err != nil {
			return fmt.Errorf("resuming retained sandbox: %w", err)
		}
		// WithAdoptExisting re-attaches to the session's claim when it
		// already exists (controller restart); without it the create
		// conflict fails instead of adopting.
		sb, err := h.f.Client.CreateSandbox(ctx, h.f.WarmPool, h.f.Namespace,
			sdk.WithClaimName(h.claimName), sdk.WithAdoptExisting())
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
	h.log.V(4).Info("Runner reachable")
	return nil
}

// StartExecutor tells the runner to run this session's executor. Failure
// classification follows the CMA bridge: transport errors and 5xx leave
// delivery uncertain (the executor may be starting); a 409 is the definite
// binding conflict that makes the session manager abandon the sandbox.
func (h *sandboxHandle) StartExecutor(ctx context.Context, ev SessionEvent, attemptID int) error {
	h.mu.Lock()
	baseURL := h.baseURL
	h.mu.Unlock()
	if baseURL == "" {
		return errors.New("runner not connected; EnsureReady must run first")
	}

	body, err := json.Marshal(runnerExec{
		SessionID:     ev.SessionID,
		EnvironmentID: ev.EnvironmentID,
		RemoteURL:     ev.RemoteURL,
		AttemptID:     fmt.Sprint(attemptID),
	})
	if err != nil {
		return fmt.Errorf("encoding executor request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/executor", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.http.Do(req)
	if err != nil {
		// Transport failure: the request may have reached the runner and the
		// response been lost, so delivery is uncertain.
		return newPostError(fmt.Errorf("POST /executor: %w", err), true)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		return newPostError(fmt.Errorf("%w: POST /executor: HTTP 409: %s", errConflictBinding, respBody), false)
	}
	if resp.StatusCode/100 != 2 {
		// 5xx leaves delivery uncertain (the runner may have started the
		// executor before failing); other statuses are definite rejections.
		return newPostError(fmt.Errorf("POST /executor: HTTP %d: %s", resp.StatusCode, respBody),
			resp.StatusCode >= 500)
	}
	if readErr != nil {
		// A 2xx whose body could not be read: the runner may well have
		// started the executor before the response was truncated.
		return newPostError(fmt.Errorf("POST /executor: reading acceptance: %w", readErr), true)
	}
	var ar struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &ar); err != nil || ar.Status == "" {
		// A 2xx with a malformed or empty body carries no definite answer;
		// treat it like a truncated acceptance.
		return newPostError(fmt.Errorf("POST /executor: malformed acceptance response %q", respBody), true)
	}
	if ar.Status != "started" && ar.Status != "already_running" {
		// The runner answered definitively without accepting.
		return newPostError(fmt.Errorf("POST /executor: runner answered %q (%s)", ar.Status, ar.Error), false)
	}
	return nil
}

// WaitConnected polls the runner until the executor process is up and has
// registered with the platform. If the deadline passes without the ready
// marker, one platform-side cross-check decides: a session that no longer
// requests an environment connection has its executor connected even if the
// runner never saw the marker (the codex CLI's output format is not a
// contract this bridge controls).
func (h *sandboxHandle) WaitConnected(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return h.connectedCrossCheck(ctx, timeout)
		case <-time.After(h.pollInterval):
		}

		st, err := h.fetchStatus(ctx)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= statusMaxConsecutiveErrors {
				return fmt.Errorf("runner unreachable: %w", err)
			}
			h.log.V(4).Info("Status poll failed, retrying", "error", err.Error())
			continue
		}
		consecutiveErrors = 0
		if !st.Alive {
			return fmt.Errorf("executor exited before connecting (exit %d): %s",
				st.ExitCode, st.StderrTail)
		}
		if st.Ready {
			return nil
		}
	}
}

func (h *sandboxHandle) connectedCrossCheck(ctx context.Context, timeout time.Duration) error {
	if h.f.API == nil {
		return fmt.Errorf("executor did not report ready within %s", timeout)
	}
	info, err := h.f.API.GetSession(ctx, h.sessionID)
	if err == nil && !info.NeedsConnection {
		h.log.Info("Executor connected (platform cross-check; ready marker not seen)")
		return nil
	}
	return fmt.Errorf("executor did not connect within %s", timeout)
}

// WaitTurnSettled polls the platform until the session's latest root turn
// reaches a terminal state, and returns that state. Connection health and
// turn success stay separate records: an error here does not imply the
// executor is down.
func (h *sandboxHandle) WaitTurnSettled(ctx context.Context) (string, error) {
	if h.f.API == nil {
		return "", errors.New("no platform API configured")
	}
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(h.turnInterval):
		}

		info, err := h.f.API.GetSession(ctx, h.sessionID)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return "", fmt.Errorf("%w while waiting for turn", err)
			}
			consecutiveErrors++
			if consecutiveErrors >= turnMaxConsecutiveErrors {
				return "", fmt.Errorf("platform unreachable: %w", err)
			}
			h.log.V(4).Info("Turn poll failed, retrying", "error", err.Error())
			continue
		}
		consecutiveErrors = 0
		if info.ActiveTurn {
			continue
		}
		switch info.LatestTurnState {
		case "completed", "failed", "cancelled":
			return info.LatestTurnState, nil
		}
		// No turn recorded yet, or a non-terminal state without ActiveTurn:
		// the harness may not have opened the turn yet. Keep waiting.
	}
}

// StopExecutor gracefully terminates the executor before suspension so a
// mid-turn process is never SIGKILLed by pod termination. Transport errors
// are logged, not returned: Suspend terminates the pod and with it the
// executor, so a lost stop call cannot leave a process behind.
func (h *sandboxHandle) StopExecutor(ctx context.Context) error {
	h.mu.Lock()
	baseURL := h.baseURL
	h.mu.Unlock()
	if baseURL == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, stopExecutorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, baseURL+"/executor/stop", nil)
	if err != nil {
		return err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.log.V(2).Info("Stop request did not reach the runner; suspension terminates the pod anyway",
			"error", err.Error())
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		h.log.V(2).Info("Runner rejected the stop request; suspension terminates the pod anyway",
			"status", resp.StatusCode)
		return nil
	}
	var sr struct {
		Status string `json:"status"`
		Forced bool   `json:"forced"`
	}
	if json.Unmarshal(body, &sr) == nil && sr.Forced {
		h.log.Info("Executor ignored SIGTERM and was killed", "session_id", h.sessionID)
	}
	return nil
}

func (h *sandboxHandle) fetchStatus(ctx context.Context) (runnerStatus, error) {
	h.mu.Lock()
	baseURL := h.baseURL
	h.mu.Unlock()
	if baseURL == "" {
		return runnerStatus{}, errors.New("runner not connected")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/status", nil)
	if err != nil {
		return runnerStatus{}, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return runnerStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return runnerStatus{}, fmt.Errorf("GET /status: HTTP %d", resp.StatusCode)
	}
	var st runnerStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return runnerStatus{}, fmt.Errorf("decoding /status: %w", err)
	}
	return st, nil
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
		// A tracked session whose handle was never opened (controller
		// restart) can still hold a running sandbox: suspend it by name
		// instead of reporting success without doing anything. No claim
		// means nothing to suspend, not an error.
		return h.suspendUnbound(ctx)
	}
	return h.sb.Suspend(ctx)
}

// resumeIfSuspended flips the session's retained sandbox back to Running
// when it is suspended, and waits for Ready to be observed at the resumed
// generation so a stale Ready=True from before the suspension cannot
// satisfy the SDK's readiness wait.
func (h *sandboxHandle) resumeIfSuspended(ctx context.Context) error {
	if h.f.K8s == nil {
		return nil
	}
	name, _, err := h.sandboxNameForClaim(ctx)
	if err != nil || name == "" {
		// No claim, or no sandbox bound yet: nothing to resume. Adoption
		// waits for the binding itself.
		return err
	}
	return h.setOperatingMode(ctx, name, sandboxv1beta1.SandboxOperatingModeRunning,
		sandboxv1beta1.SandboxConditionReady, "Resuming suspended sandbox before adoption")
}

// suspendUnbound suspends the sandbox the session's claim holds without
// opening an SDK handle. A missing claim or sandbox is success: there is
// nothing to suspend. A claim that exists but has no sandbox bound yet is
// a retryable failure: the binding can complete after a false success and
// leave a running pod marked suspended.
func (h *sandboxHandle) suspendUnbound(ctx context.Context) error {
	if h.f.K8s == nil {
		return nil
	}
	name, found, err := h.sandboxNameForClaim(ctx)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if name == "" {
		return fmt.Errorf("claim %s exists but has no sandbox bound yet", h.claimName)
	}
	return h.setOperatingMode(ctx, name, sandboxv1beta1.SandboxOperatingModeSuspended,
		sandboxv1beta1.SandboxConditionSuspended, "Suspending retained sandbox")
}

// sandboxNameForClaim resolves the Sandbox backing the session's claim.
// found is false when the claim does not exist; name is empty when the
// claim has no sandbox bound yet.
func (h *sandboxHandle) sandboxNameForClaim(ctx context.Context) (name string, found bool, err error) {
	claim, err := h.f.K8s.ExtensionsClient.SandboxClaims(h.f.Namespace).Get(ctx, h.claimName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("getting claim %s: %w", h.claimName, err)
	}
	return claim.Status.SandboxStatus.Name, true, nil
}

// setOperatingMode patches the Sandbox's operatingMode when it differs and
// waits for wantCond to be observed at the patched generation.
func (h *sandboxHandle) setOperatingMode(ctx context.Context, name string,
	mode sandboxv1beta1.SandboxOperatingMode, wantCond sandboxv1beta1.ConditionType, logMsg string) error {
	cctx, cancel := context.WithTimeout(ctx, operatingModeReadyTimeout)
	defer cancel()

	sbx, err := h.f.K8s.AgentsClient.Sandboxes(h.f.Namespace).Get(cctx, name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting sandbox %s: %w", name, err)
	}
	minGeneration := sbx.Generation
	if sbx.Spec.OperatingMode != mode {
		patch := []byte(fmt.Sprintf(`{"spec":{"operatingMode":%q}}`, mode))
		patched, err := h.f.K8s.AgentsClient.Sandboxes(h.f.Namespace).
			Patch(cctx, name, k8stypes.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("setting operatingMode %s on sandbox %s: %w", mode, name, err)
		}
		minGeneration = patched.Generation
		h.log.Info(logMsg, "sandbox", name)
	}

	for {
		sbx, err := h.f.K8s.AgentsClient.Sandboxes(h.f.Namespace).Get(cctx, name, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// Deleted concurrently: the goal state is unreachable but
				// also moot; let the caller's next step surface it.
				return nil
			}
			return fmt.Errorf("getting sandbox %s: %w", name, err)
		}
		if sbx.Spec.OperatingMode != mode {
			// Flipped back mid-wait: fail retryably rather than accept a
			// condition that no longer describes the requested mode.
			return fmt.Errorf("sandbox %s operatingMode is %s, want %s",
				name, sbx.Spec.OperatingMode, mode)
		}
		// The condition must be True at the object's current generation: an
		// older observedGeneration can carry a stale status from before the
		// mode change.
		for _, c := range sbx.Status.Conditions {
			if c.Type == string(wantCond) && c.Status == metav1.ConditionTrue &&
				c.ObservedGeneration == sbx.Generation {
				return nil
			}
		}
		select {
		case <-cctx.Done():
			return fmt.Errorf("sandbox %s did not report %s at generation %d: %w",
				name, wantCond, minGeneration, cctx.Err())
		case <-time.After(h.pollInterval):
		}
	}
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
		// The claim can still exist (a restarted controller tracks the
		// session before ever opening a handle): delete it by name. A
		// missing claim is not an error, so retries stay safe.
		return h.f.Client.DeleteSandbox(ctx, h.claimName, h.f.Namespace)
	}
	if err := h.sb.Close(ctx); err != nil {
		return err
	}
	h.sb = nil
	return nil
}
