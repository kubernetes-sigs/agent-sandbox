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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// lifecycleTimeout bounds one sandbox lifecycle operation (suspend, release,
// executor stop) so a stuck apiserver call cannot wedge a session loop.
const lifecycleTimeout = 60 * time.Second

// errConflictBinding reports the runner's 409: the sandbox's executor is
// bound to a different session. Unlike the CMA bridge's transient busy-409,
// an OpenAI executor binding is long-lived, so retrying the same sandbox
// would never succeed. The session loop abandons the sandbox instead: it
// releases the claim and retries the event on a freshly claimed one.
var errConflictBinding = errors.New("executor bound to a different session")

// ErrExecutorFailed reports that a session reached the terminal
// executor_failed state (repeated connect timeouts or an executor that dies
// at startup). The reconciler stops dispatching it; GC still releases the
// sandbox on schedule.
var ErrExecutorFailed = errors.New("session executor failed terminally")

// postError classifies a StartExecutor failure. uncertain means the delivery
// state is unknown (transport failure or 5xx): the runner may have started
// the executor before the response was lost.
type postError struct {
	err       error
	uncertain bool
}

func newPostError(err error, uncertain bool) error {
	return &postError{err: err, uncertain: uncertain}
}

func (e *postError) Error() string { return e.err.Error() }
func (e *postError) Unwrap() error { return e.err }

// IsUncertainPost reports whether a StartExecutor failure left delivery
// uncertain, in which case the runner may already run the executor and
// lifecycle cleanup must stay off.
func IsUncertainPost(err error) bool {
	var pe *postError
	return errors.As(err, &pe) && pe.uncertain
}

// sessionSandbox drives the sandbox lifecycle for one Agents API session.
// sandboxHandle (sandboxhandle.go) implements it.
type sessionSandbox interface {
	// EnsureReady claims the sandbox if needed, resumes it if suspended,
	// and establishes connectivity to its runner.
	EnsureReady(ctx context.Context) error
	// StartExecutor tells the runner to run the session's executor. A 409
	// comes back wrapping errConflictBinding; transport ambiguity comes back
	// as an uncertain postError.
	StartExecutor(ctx context.Context, ev SessionEvent, attemptID int) error
	// WaitConnected blocks until the executor is up and registered with the
	// platform, or the timeout passes.
	WaitConnected(ctx context.Context, timeout time.Duration) error
	// WaitTurnSettled blocks until the session's turn reaches a terminal
	// state and returns that state ("completed", "failed", ...).
	WaitTurnSettled(ctx context.Context) (string, error)
	// StopExecutor gracefully terminates the executor before suspension.
	StopExecutor(ctx context.Context) error
	// Suspend tears down connectivity and suspends the sandbox.
	Suspend(ctx context.Context) error
	// Release deletes the claim and drops all sandbox state.
	Release(ctx context.Context) error
}

const (
	defaultQueueLen           = 64
	defaultProcessAttempts    = 5
	defaultRetryBase          = 2 * time.Second
	defaultRunnerActiveWindow = 15 * time.Minute
	defaultConnectTimeout     = 4 * time.Minute
)

// queueItem is one entry of a session's FIFO queue: either a session event
// or a suspend request. Suspend requests go through the queue so suspension
// is serialized with turn execution and can never terminate a pod mid-turn.
type queueItem struct {
	ev      SessionEvent
	suspend bool
	// idleFor is the idle duration the suspend request requires when it is
	// processed; work that ran since the GC decision cancels the suspension.
	idleFor time.Duration
}

// SessionManagerConfig configures a SessionManager. Zero values select the
// defaults; tests shrink the retry and timeout parameters.
type SessionManagerConfig struct {
	// Factory builds the sandbox handle for a session ID.
	Factory func(sessionID string) sessionSandbox
	Logger  logr.Logger
	// Now reports the current time; defaults to time.Now.
	Now func() time.Time
	// RetryBase is the first backoff delay when processing an event fails.
	RetryBase time.Duration
	// ProcessAttempts bounds retries before an event is abandoned. The
	// platform keeps the required action until it is served, so the next
	// poll re-delivers it.
	ProcessAttempts int
	// QueueLen is the per-session pending-event limit.
	QueueLen int
	// ConnectTimeout bounds WaitConnected. It must stay below the
	// platform's five-minute input-time connection window.
	ConnectTimeout time.Duration
	// SessionActiveCheck asks the platform whether a session still has an
	// active turn or a pending required action. GC suspensions abort when it
	// says yes or when the check itself fails: the platform warns that an
	// idle notification can arrive just before waiting input starts
	// executing. Nil disables the check (tests).
	SessionActiveCheck func(ctx context.Context, sessionID string) (bool, error)
	// RunnerActiveWindow bounds how long an unconfirmed turn (executor
	// connected, completion never observed) keeps suspension and release
	// off. The executor may still be running it after the controller gave
	// up waiting; the window prevents terminating that pod while keeping a
	// never-finishing turn from blocking cleanup forever.
	RunnerActiveWindow time.Duration
}

// SessionManager routes session events to one serializing loop per Agents
// API session. Events for the same session never overlap; different sessions
// run concurrently. The OpenAI protocol has no work queue: the reconciler
// re-delivers the environment_connection action every poll until it is
// served, and this manager deduplicates it per session.
type SessionManager struct {
	factory        func(sessionID string) sessionSandbox
	log            logr.Logger
	now            func() time.Time
	retryBase      time.Duration
	attempts       int
	queueLen       int
	connectTimeout time.Duration
	activeCheck    func(ctx context.Context, sessionID string) (bool, error)
	activeWindow   time.Duration

	ctx context.Context

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// NewSessionManager creates the manager and binds session loops to ctx.
func NewSessionManager(ctx context.Context, cfg SessionManagerConfig) *SessionManager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = defaultRetryBase
	}
	if cfg.ProcessAttempts <= 0 {
		cfg.ProcessAttempts = defaultProcessAttempts
	}
	if cfg.QueueLen <= 0 {
		cfg.QueueLen = defaultQueueLen
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.RunnerActiveWindow <= 0 {
		cfg.RunnerActiveWindow = defaultRunnerActiveWindow
	}
	return &SessionManager{
		factory:        cfg.Factory,
		log:            cfg.Logger,
		now:            cfg.Now,
		retryBase:      cfg.RetryBase,
		attempts:       cfg.ProcessAttempts,
		queueLen:       cfg.QueueLen,
		connectTimeout: cfg.ConnectTimeout,
		activeCheck:    cfg.SessionActiveCheck,
		activeWindow:   cfg.RunnerActiveWindow,
		ctx:            ctx,
		sessions:       make(map[string]*sessionState),
	}
}

// Dispatch routes an event to its session, creating the session on first
// sight. It returns ErrAlreadyActive when the session already has the event
// in flight or queued — the reconciler treats that as idempotent
// consumption. It returns an error when the session is full, terminally
// failed, or being released; the platform re-delivers the action on the next
// poll.
func (m *SessionManager) Dispatch(ev SessionEvent) error {
	m.mu.Lock()
	s, ok := m.sessions[ev.SessionID]
	if !ok {
		s = m.newSessionStateLocked(ev.SessionID)
	}
	m.mu.Unlock()

	return s.enqueue(queueItem{ev: ev}, m.queueLen)
}

// Track registers a managed session that needs no executor connection, so
// GC still cleans up a sandbox it holds (e.g. after a controller restart).
// Unlike Dispatch it starts no work; the handle claims lazily if the
// session ever needs a connection again.
func (m *SessionManager) Track(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		m.newSessionStateLocked(sessionID)
	}
}

// newSessionStateLocked builds a session's serial loop, registers it, and
// starts its goroutine. The caller holds m.mu.
func (m *SessionManager) newSessionStateLocked(sessionID string) *sessionState {
	sctx, cancel := context.WithCancel(m.ctx)
	s := &sessionState{
		id:           sessionID,
		sb:           m.factory(sessionID),
		newHandle:    func() sessionSandbox { return m.factory(sessionID) },
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
		log:          m.log.WithValues("session_id", sessionID),
		lastDone:     m.now(),
		cancel:       cancel,
		activeWindow: m.activeWindow,
		connectTO:    m.connectTimeout,
		activeCheck:  m.activeCheck,
	}
	m.sessions[sessionID] = s
	go s.run(sctx, m.retryBase, m.attempts, m.now)
	return s
}

// RemoveSession drops a session the platform reports deleted and releases
// its sandbox. Deleting an Agents API session does not reclaim self-hosted
// compute; this is the bridge's side of that cleanup.
func (m *SessionManager) RemoveSession(sessionID string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return
	}
	// Stopping first keeps the retained entry from accepting new work when
	// the release below fails and the session stays in the map for a GC
	// retry.
	s.stop()
	rctx, cancel := context.WithTimeout(m.ctx, lifecycleTimeout)
	defer cancel()
	if err := s.sb.Release(rctx); err != nil {
		m.log.Error(err, "Releasing sandbox of deleted session failed; GC will retry",
			"session_id", sessionID)
		return
	}
	m.mu.Lock()
	if cur, ok := m.sessions[sessionID]; ok && cur == s {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
}

// ReleaseInactive releases the sandbox of every tracked session the
// platform reports inactive and keeps the rest. It is the shutdown cleanup:
// a claim of an active or unknown-state session must survive because
// deleting it mid-turn destroys a workspace the platform still expects.
// A nil active check releases everything.
func (m *SessionManager) ReleaseInactive(ctx context.Context) {
	for _, s := range m.list() {
		if m.activeCheck != nil {
			active, err := m.activeCheck(ctx, s.id)
			if err != nil {
				m.log.Info("Keeping claim: platform state check failed",
					"session_id", s.id, "error", err.Error())
				continue
			}
			if active {
				m.log.Info("Keeping claim: session still active", "session_id", s.id)
				continue
			}
		}
		s.stop()
		if err := s.sb.Release(ctx); err != nil {
			m.log.Error(err, "Releasing sandbox failed", "session_id", s.id)
			continue
		}
		m.remove(s.id)
	}
}

// list snapshots the sessions for the GC loop.
func (m *SessionManager) list() []*sessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*sessionState, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// remove drops a session and stops its goroutines. A later event recreates
// it from scratch.
func (m *SessionManager) remove(sessionID string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

// sessionState is one session's serial execution loop and lifecycle state.
// All mutable fields are guarded by mu.
type sessionState struct {
	id        string
	sb        sessionSandbox
	newHandle func() sessionSandbox // rebuilds sb after an abandoned sandbox
	log       logr.Logger
	wake      chan struct{} // buffered 1; edge signal for the run loop
	done      chan struct{} // closed when the run loop exits
	cancel    context.CancelFunc

	connectTO   time.Duration
	activeCheck func(ctx context.Context, sessionID string) (bool, error)

	mu       sync.Mutex
	pending  []queueItem
	inFlight *SessionEvent // popped event being processed; non-nil means busy
	// attempt is the monotonic runner attempt ID; it is never reset.
	attempt int
	// connectFails counts consecutive connection failures; two in a row put
	// the session in the terminal executor_failed state. Tracked separately
	// from attempt, which never resets and would fail long-lived sessions on
	// their first bad reconnect.
	connectFails int
	failed       bool // terminal executor_failed state
	abandons     int  // sandbox abandons (409) for the current event
	reserved     bool // GC reserved the session for release
	stopped      bool
	lastDone     time.Time
	// turnActive marks a connected executor whose turn the controller has
	// not seen settle yet; it opens the runner-active window.
	turnActive bool
	suspended  bool
	// runnerActiveUntil bounds how long a turn the executor accepted but the
	// controller never saw finish keeps suspension and release off. Zero when
	// no unconfirmed turn is outstanding.
	runnerActiveUntil time.Time
	activeWindow      time.Duration
}

// enqueue appends an item unless the session is full, terminally failed, or
// being released. A duplicate of the in-flight or queued event is folded
// away with ErrAlreadyActive: the reconciler polls every few seconds while
// the action is pending, and the loop must not queue the same connection
// request repeatedly.
func (s *sessionState) enqueue(item queueItem, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return fmt.Errorf("session %s: %w", s.id, ErrExecutorFailed)
	}
	if s.stopped || s.reserved {
		return fmt.Errorf("session %s is being released; the platform re-delivers the action", s.id)
	}
	if !item.suspend {
		if s.inFlight != nil && s.inFlight.SessionID == item.ev.SessionID {
			return ErrAlreadyActive
		}
		for _, p := range s.pending {
			if !p.suspend && p.ev.SessionID == item.ev.SessionID {
				return ErrAlreadyActive
			}
		}
	}
	if len(s.pending) >= limit {
		return fmt.Errorf("session %s queue full; the platform re-delivers the action", s.id)
	}
	s.pending = append(s.pending, item)
	s.signalWake()
	return nil
}

// signalWake nudges the run loop. The caller holds s.mu.
func (s *sessionState) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// requestSuspend queues a suspend request that the session loop executes
// only if the session is still idle for idleFor when the request is
// processed. The request deliberately queues behind in-flight work:
// serialization is what guarantees suspension never terminates a pod
// mid-turn. Returns false when the session is gone or a request is already
// queued.
func (s *sessionState) requestSuspend(idleFor time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.reserved || s.suspended {
		return false
	}
	for _, item := range s.pending {
		if item.suspend {
			return false
		}
	}
	s.pending = append(s.pending, queueItem{suspend: true, idleFor: idleFor})
	s.signalWake()
	return true
}

// run drains the queue until the session context ends.
func (s *sessionState) run(ctx context.Context, retryBase time.Duration, attempts int, now func() time.Time) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			item, ok := s.pop()
			if !ok {
				break
			}
			if item.suspend {
				s.runSuspend(ctx, item.idleFor)
				continue
			}
			if err := s.process(ctx, item.ev, retryBase, attempts); err != nil {
				s.log.Error(err, "Session event abandoned; the platform re-delivers the action",
					"session_id", item.ev.SessionID)
			}
			s.finishEvent(now())
		}
	}
}

// runSuspend executes a queued suspend request. The idleness requirement is
// rechecked here, not when the GC queued it: work that ran in between makes
// the suspension obsolete. The platform-side check catches the race the
// OpenAI docs warn about — an idle notification can arrive just before
// waiting input begins executing.
func (s *sessionState) runSuspend(ctx context.Context, idleFor time.Duration) {
	s.mu.Lock()
	queuedBehind := len(s.pending) > 0
	idle := time.Since(s.lastDone)
	activeUntil := s.runnerActiveUntil
	s.mu.Unlock()
	if queuedBehind || idle < idleFor {
		return
	}
	if time.Now().Before(activeUntil) {
		s.log.Info("Suspend skipped: the executor may still be running an unconfirmed turn",
			"blocked_until", activeUntil.UTC().Format(time.RFC3339))
		return
	}
	if s.activeCheck != nil {
		cctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
		active, err := s.activeCheck(cctx, s.id)
		cancel()
		if err != nil {
			// Conservative: unknown platform state keeps the sandbox up.
			s.log.V(2).Info("Suspend skipped: platform state check failed", "error", err.Error())
			return
		}
		if active {
			s.log.V(2).Info("Suspend skipped: the platform reports the session active")
			return
		}
	}

	sctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	// Terminate the executor before the pod: a SIGKILLed mid-turn process
	// leaves the platform with a half-finished turn, while a graceful stop
	// lets it record the cancellation cleanly.
	if err := s.sb.StopExecutor(sctx); err != nil {
		s.log.V(2).Info("Stopping executor before suspend failed; suspending anyway", "error", err.Error())
	}
	if err := s.sb.Suspend(sctx); err != nil {
		s.log.Error(err, "Suspending idle sandbox failed")
		return
	}
	s.markSuspended()
	s.log.Info("Suspended idle session sandbox", "idle", idle.Round(time.Second))
}

// pop takes the next item and marks the session busy.
func (s *sessionState) pop() (queueItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.pending) == 0 {
		return queueItem{}, false
	}
	item := s.pending[0]
	s.pending = s.pending[1:]
	if !item.suspend {
		ev := item.ev
		s.inFlight = &ev
	}
	return item, true
}

// markTurnActive opens the runner-active window when the executor connects:
// from here on a turn may be executing, and if the controller never sees it
// settle, cleanup must stay off until the window passes.
func (s *sessionState) markTurnActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnActive = true
	s.runnerActiveUntil = time.Now().Add(s.activeWindow)
}

// finishTurn closes the runner-active window after a confirmed terminal
// outcome and records it. On error the window stays untouched: the turn may
// still be executing, and process relies on turnActive to keep lifecycle
// cleanup off during retries. Connection health and turn success are
// separate records: a failed turn on a healthy executor is the application's
// problem, not the bridge's, so it only logs.
func (s *sessionState) finishTurn(state string, err error) {
	if err != nil {
		s.log.V(2).Info("Turn outcome unknown", "error", err.Error())
		return
	}
	s.mu.Lock()
	s.turnActive = false
	s.runnerActiveUntil = time.Time{}
	s.mu.Unlock()
	switch state {
	case "completed":
		s.log.Info("Turn completed", "turn_state", state)
	case "":
	default:
		s.log.Info("Turn ended unsuccessfully; the executor stays connected",
			"turn_state", state)
	}
}

// runnerMayBeActive reports whether an unconfirmed turn may still be
// executing in the sandbox.
func (s *sessionState) runnerMayBeActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.runnerActiveUntil)
}

// refreshRunnerActiveWindow restarts the protection window from now and
// returns the new deadline.
func (s *sessionState) refreshRunnerActiveWindow() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerActiveUntil = time.Now().Add(s.activeWindow)
	return s.runnerActiveUntil
}

// finishEvent commits one event's completion: the session goes idle and is
// awake (work just ran, which implies a resumed or fresh sandbox).
func (s *sessionState) finishEvent(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = nil
	s.lastDone = at
	s.suspended = false
}

// process runs one event with bounded retries.
func (s *sessionState) process(ctx context.Context, ev SessionEvent, retryBase time.Duration, attempts int) error {
	delay := retryBase
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = s.runOnce(ctx, ev); err == nil {
			return nil
		}
		if errors.Is(err, errConflictBinding) {
			// The sandbox belongs to another session (a claim/binding bug):
			// release it and retry the event on a fresh claim. Bounded so a
			// systemic mislabeling cannot spin forever.
			if rerr := s.abandonSandbox(ctx); rerr != nil {
				// Keep the handle: the retry hits the same conflict and gets
				// another chance to release it.
				s.log.Error(rerr, "Releasing conflicting sandbox failed; retrying",
					"session_id", s.id)
				continue
			}
			s.mu.Lock()
			s.abandons++
			abandons := s.abandons
			s.mu.Unlock()
			if abandons >= 2 {
				s.markFailed()
				return fmt.Errorf("session %s: binding conflict persists after re-claim: %w", s.id, err)
			}
			continue
		}
		s.log.Error(err, "Session event processing failed",
			"session_id", ev.SessionID, "attempt", attempt, "of", attempts)
		if attempt == attempts || s.isFailed() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	s.mu.Lock()
	turnActive := s.turnActive
	s.mu.Unlock()
	if turnActive {
		// The executor connected but the controller never saw the turn
		// settle. Measure the protection window from this abandonment: a
		// turn that ran longer than the window would otherwise leave the
		// pod unprotected while still executing.
		until := s.refreshRunnerActiveWindow()
		s.log.Info("Event abandoned while a turn may still be executing; lifecycle cleanup paused",
			"session_id", ev.SessionID, "blocked_until", until.UTC().Format(time.RFC3339))
	}
	return fmt.Errorf("session %s event failed after %d attempts: %w", ev.SessionID, attempts, err)
}

// runOnce brings the sandbox up, starts the executor, and waits for the
// turn to settle.
func (s *sessionState) runOnce(ctx context.Context, ev SessionEvent) error {
	if err := s.sb.EnsureReady(ctx); err != nil {
		return fmt.Errorf("ensuring sandbox ready: %w", err)
	}
	s.mu.Lock()
	s.attempt++
	attempt := s.attempt
	s.mu.Unlock()

	if err := s.sb.StartExecutor(ctx, ev, attempt); err != nil {
		if errors.Is(err, errConflictBinding) {
			return err // process() abandons the sandbox and retries
		}
		if IsUncertainPost(err) {
			// The runner may have started the executor before its response
			// was lost; keep lifecycle cleanup off until the window passes.
			s.markTurnActive()
		}
		return fmt.Errorf("starting executor: %w", err)
	}
	if err := s.sb.WaitConnected(ctx, s.connectTO); err != nil {
		// One retry, then terminal: a connect timeout is usually a
		// credential or egress problem that re-connecting cannot fix. Count
		// consecutive failures, not the persistent runner attempt ID.
		s.mu.Lock()
		s.connectFails++
		fails := s.connectFails
		s.mu.Unlock()
		if fails >= 2 {
			s.markFailed()
		}
		return fmt.Errorf("waiting for executor connection: %w", err)
	}
	s.mu.Lock()
	s.connectFails = 0
	s.mu.Unlock()
	s.markTurnActive()
	s.log.Info("Executor connected", "session_id", ev.SessionID, "attempt", attempt)

	state, err := s.sb.WaitTurnSettled(ctx)
	s.finishTurn(state, err)
	if err != nil {
		return fmt.Errorf("waiting for turn to settle: %w", err)
	}
	return nil
}

// abandonSandbox releases the conflicting sandbox and rebuilds the handle so
// the next attempt claims a fresh one. The executor inside belongs to
// another session; the bridge must not stop it, so no StopExecutor here —
// Release deletes this bridge's claim only. On release failure it returns
// the error and keeps the current handle, so a retry can release it again.
func (s *sessionState) abandonSandbox(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	if err := s.sb.Release(rctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.sb = s.newHandle()
	s.mu.Unlock()
	s.log.Info("Abandoned sandbox after binding conflict; reclaimed on retry", "session_id", s.id)
	return nil
}

func (s *sessionState) markFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	s.log.Error(nil, "Session entered terminal executor_failed state; GC will release the sandbox")
}

func (s *sessionState) isFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// idleSince reports how long the session has been idle.
func (s *sessionState) idleSince(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastDone)
}

// isBusy reports whether the session has work in flight or queued, or is
// reserved for release. GC skips such sessions.
func (s *sessionState) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight != nil || s.hasWorkLocked() || s.reserved
}

// hasWorkLocked reports whether real work (not a suspend request) is queued.
func (s *sessionState) hasWorkLocked() bool {
	for _, item := range s.pending {
		if !item.suspend {
			return true
		}
	}
	return false
}

// tryReserveRelease atomically claims an idle session for release,
// rechecking the idle threshold so work that finished after the GC's sweep
// started keeps the session alive. While reserved, enqueue rejects new
// events (the platform re-delivers the action), so release cannot race a
// session back into activity. A terminally failed session releases on the
// normal schedule: nothing will ever use its sandbox again.
func (s *sessionState) tryReserveRelease(now time.Time, releaseAfter time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight != nil || s.reserved {
		return false
	}
	// Queued work blocks a live session, but not a stopped one: a stopped
	// entry left in the map is a session whose release failed, and its
	// pending items will never drain — GC must still be able to reserve it
	// for a release retry.
	if len(s.pending) > 0 && !s.stopped {
		return false
	}
	if now.Sub(s.lastDone) < releaseAfter {
		return false
	}
	if now.Before(s.runnerActiveUntil) {
		return false
	}
	s.reserved = true
	return true
}

// unreserve returns a reserved session to service after a failed release.
func (s *sessionState) unreserve() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = false
}

func (s *sessionState) markSuspended() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspended = true
}

func (s *sessionState) suspendedState() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// stop ends the session's goroutines.
func (s *sessionState) stop() {
	s.mu.Lock()
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
