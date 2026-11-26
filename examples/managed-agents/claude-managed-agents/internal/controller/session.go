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

// WorkEvent is one claimed CMA work item routed to its session.
type WorkEvent struct {
	SessionID string
	WorkID    string
	// LastHeartbeat is the lease token from the poller's first heartbeat.
	// The runner's worker quotes it to continue the lease. The renew loop
	// replaces it while the event is queued or in flight but not yet posted.
	LastHeartbeat string
}

// LeaseRenewer extends the lease of a work item the runner has not taken
// over yet and returns the new token. *Poller implements it.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, workID, lastHeartbeat string) (string, error)
}

// postError classifies a PostWork failure. uncertain means the delivery
// state is unknown (transport failure or 5xx): the runner may have accepted
// the work and started executing it before the response was lost.
type postError struct {
	err       error
	uncertain bool
}

func newPostError(err error, uncertain bool) error {
	return &postError{err: err, uncertain: uncertain}
}

func (e *postError) Error() string { return e.err.Error() }
func (e *postError) Unwrap() error { return e.err }

// IsUncertainPost reports whether a PostWork failure left delivery
// uncertain, in which case the runner may be executing the work and
// lifecycle cleanup must stay off.
func IsUncertainPost(err error) bool {
	var pe *postError
	return errors.As(err, &pe) && pe.uncertain
}

// sessionSandbox drives the sandbox lifecycle for a single CMA session.
type sessionSandbox interface {
	// EnsureReady claims the sandbox if needed, resumes it if suspended,
	// and establishes connectivity to its runner.
	EnsureReady(ctx context.Context) error
	// PostWork tells the runner to execute a work item.
	PostWork(ctx context.Context, ev WorkEvent) error
	// WaitWorkFinished blocks until the runner reports the work is done.
	WaitWorkFinished(ctx context.Context) error
	// Suspend tears down connectivity and suspends the sandbox.
	Suspend(ctx context.Context) error
	// Release deletes the claim and drops all session state.
	Release(ctx context.Context) error
}

const (
	defaultQueueLen           = 64
	defaultProcessAttempts    = 5
	defaultRetryBase          = 2 * time.Second
	defaultRenewInterval      = 60 * time.Second
	defaultRunnerActiveWindow = 15 * time.Minute
)

// queueItem is one entry of a session's FIFO queue: either a work event or
// a suspend request. Suspend requests go through the queue so suspension is
// serialized with work execution and can never terminate a pod mid-turn.
type queueItem struct {
	ev      WorkEvent
	suspend bool
	// idleFor is the idle duration the suspend request requires when it is
	// processed; work that ran since the GC decision cancels the suspension.
	idleFor time.Duration
}

// SessionManagerConfig configures a SessionManager. Zero values select the
// defaults; tests shrink the retry and renew parameters.
type SessionManagerConfig struct {
	// Factory builds the sandbox handle for a new session ID.
	Factory func(sessionID string) sessionSandbox
	Logger  logr.Logger
	// Now reports the current time; defaults to time.Now.
	Now func() time.Time
	// RetryBase is the first backoff delay when processing an event fails.
	RetryBase time.Duration
	// ProcessAttempts bounds retries before an event is abandoned (its lease
	// lapses and the CMA API re-delivers it).
	ProcessAttempts int
	// QueueLen is the per-session pending-event limit.
	QueueLen int
	// Renewer keeps queued and in-flight (not yet posted) leases alive.
	// Nil disables renewal.
	Renewer LeaseRenewer
	// RenewInterval is how often those leases are renewed. It should stay
	// below the poller's hand-off TTL.
	RenewInterval time.Duration
	// RunnerActiveWindow bounds how long an unconfirmed turn (posted to the
	// runner, completion never observed) keeps suspension and release off.
	// The runner may still be executing it after the controller gave up
	// waiting; the window prevents terminating that pod while keeping a
	// never-finishing runner from blocking cleanup forever.
	RunnerActiveWindow time.Duration
}

// SessionManager routes work events to one serializing loop per CMA session.
// Events for the same session never overlap; different sessions run
// concurrently. It replaces the upstream Valkey queue: with work items acked
// and leased at poll time, an in-process queue gives the same per-session
// serialization for this single-process example.
type SessionManager struct {
	factory       func(sessionID string) sessionSandbox
	log           logr.Logger
	now           func() time.Time
	retryBase     time.Duration
	attempts      int
	queueLen      int
	renewer       LeaseRenewer
	renewInterval time.Duration
	activeWindow  time.Duration

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
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = defaultRenewInterval
	}
	if cfg.RunnerActiveWindow <= 0 {
		cfg.RunnerActiveWindow = defaultRunnerActiveWindow
	}
	return &SessionManager{
		factory:       cfg.Factory,
		log:           cfg.Logger,
		now:           cfg.Now,
		retryBase:     cfg.RetryBase,
		attempts:      cfg.ProcessAttempts,
		queueLen:      cfg.QueueLen,
		renewer:       cfg.Renewer,
		renewInterval: cfg.RenewInterval,
		activeWindow:  cfg.RunnerActiveWindow,
		ctx:           ctx,
		sessions:      make(map[string]*sessionState),
	}
}

// Dispatch routes an event to its session, creating the session on first
// sight. It returns an error when the session is full or being released; the
// caller leaves the item's lease to lapse so the API re-delivers it.
func (m *SessionManager) Dispatch(ev WorkEvent) error {
	m.mu.Lock()
	s, ok := m.sessions[ev.SessionID]
	if !ok {
		sctx, cancel := context.WithCancel(m.ctx)
		s = &sessionState{
			id:           ev.SessionID,
			sb:           m.factory(ev.SessionID),
			wake:         make(chan struct{}, 1),
			done:         make(chan struct{}),
			log:          m.log.WithValues("session_id", ev.SessionID),
			lastDone:     m.now(),
			cancel:       cancel,
			activeWindow: m.activeWindow,
		}
		s.cond = sync.NewCond(&s.mu)
		m.sessions[ev.SessionID] = s
		go s.run(sctx, m.retryBase, m.attempts, m.now)
		go s.renewLoop(sctx, m.renewer, m.renewInterval)
	}
	m.mu.Unlock()

	return s.enqueue(queueItem{ev: ev}, m.queueLen)
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

// sessionState is one CMA session's serial execution loop and lifecycle
// state. All mutable fields are guarded by mu.
type sessionState struct {
	id     string
	sb     sessionSandbox
	log    logr.Logger
	wake   chan struct{} // buffered 1; edge signal for the run loop
	done   chan struct{} // closed when the run loop exits
	cancel context.CancelFunc

	mu         sync.Mutex
	cond       *sync.Cond // signaled when a renewal commits; takeForPost waits
	pending    []queueItem
	busy       bool
	inFlight   *WorkEvent // popped event being processed, before it is posted
	posted     bool       // the runner owns the lease for inFlight
	posting    bool       // PostWork call in progress; renewal paused
	renewingID string     // work ID with a RenewLease call in progress, if any
	leaseLost  bool       // inFlight's lease could not be renewed
	reserved   bool       // GC reserved the session for release
	stopped    bool
	lastDone   time.Time
	suspended  bool
	// runnerActiveUntil bounds how long a turn the runner accepted but the
	// controller never saw finish (abandoned after retries) keeps suspend
	// and release off. Zero when no unconfirmed turn is outstanding.
	runnerActiveUntil time.Time
	activeWindow      time.Duration
}

// enqueue appends an item unless the session is full or being released.
func (s *sessionState) enqueue(item queueItem, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.reserved {
		return fmt.Errorf("session %s is being released; leaving work %s for lease-based re-delivery", s.id, item.ev.WorkID)
	}
	if len(s.pending) >= limit {
		return fmt.Errorf("session %s queue full; dropping work %s for lease-based re-delivery", s.id, item.ev.WorkID)
	}
	s.pending = append(s.pending, item)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// requestSuspend queues a suspend request that the session loop executes
// only if the session is still idle for idleFor when the request is
// processed. Returns false when the session is gone or a request is already
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
	select {
	case s.wake <- struct{}{}:
	default:
	}
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
				s.log.Error(err, "Work item abandoned; its lease will lapse for re-delivery",
					"work_id", item.ev.WorkID)
			}
			s.finishEvent(now())
		}
	}
}

// runSuspend executes a queued suspend request. The idleness requirement is
// rechecked here, not when the GC queued it: work that ran in between makes
// the suspension obsolete.
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
		s.log.Info("Suspend skipped: the runner may still be executing an unconfirmed turn",
			"blocked_until", activeUntil.UTC().Format(time.RFC3339))
		return
	}

	sctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	if err := s.sb.Suspend(sctx); err != nil {
		s.log.Error(err, "Suspending idle sandbox failed")
		return
	}
	s.markSuspended()
	s.log.Info("Suspended idle session sandbox", "idle", idle.Round(time.Second))
}

// pop takes the next item. For work items it moves the event to inFlight,
// where the renew loop keeps its lease alive until PostWork succeeds.
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
		s.posted = false
		s.posting = false
		s.leaseLost = false
		s.busy = true
	}
	return item, true
}

// takeForPost returns the current lease token for the in-flight event and
// pauses renewal for it, so the token cannot change between this read and
// the runner receiving it. It waits for any renewal of the same work ID to
// commit first — including one that started while the event was still
// queued — because handing the runner a token that a concurrent RenewLease
// is about to rotate would make its first heartbeat fail with 412.
func (s *sessionState) takeForPost() (WorkEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == nil {
		return WorkEvent{}, errors.New("no in-flight event")
	}
	for s.renewingID != "" && s.renewingID == s.inFlight.WorkID {
		s.cond.Wait()
	}
	if s.leaseLost {
		return WorkEvent{}, errors.New("lease could not be renewed; leaving the item for re-delivery")
	}
	s.posting = true
	return *s.inFlight, nil
}

// applyRenewal commits one renewal result. The item may sit in pending, or
// it may have transitioned to inFlight while the renewal was in flight; both
// placements are checked so the new token is never discarded. The caller
// must have set renewingID to workID before starting the call.
func (s *sessionState) applyRenewal(workID, tok string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewingID = ""
	applied := false
	for i := range s.pending {
		if s.pending[i].suspend || s.pending[i].ev.WorkID != workID {
			continue
		}
		if err != nil {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
		} else {
			s.pending[i].ev.LastHeartbeat = tok
		}
		applied = true
		break
	}
	if !applied && s.inFlight != nil && s.inFlight.WorkID == workID && !s.posted {
		if err != nil {
			s.leaseLost = true
		} else {
			s.inFlight.LastHeartbeat = tok
		}
	}
	s.cond.Broadcast()
}

// markPosted records that the runner owns the lease now, and opens the
// runner-active window: if the controller never observes the turn finish,
// the runner may still be executing it, and lifecycle cleanup must stay off
// until the window passes.
func (s *sessionState) markPosted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posted = true
	s.posting = false
	s.runnerActiveUntil = time.Now().Add(s.activeWindow)
}

// confirmCompletion closes the runner-active window after the runner
// reported the turn finished.
func (s *sessionState) confirmCompletion() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerActiveUntil = time.Time{}
}

// runnerMayBeActive reports whether an unconfirmed turn may still be
// executing in the sandbox.
func (s *sessionState) runnerMayBeActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.runnerActiveUntil)
}

// turnPosted reports whether the runner accepted (or may have accepted) the
// current event.
func (s *sessionState) turnPosted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posted
}

// refreshRunnerActiveWindow restarts the protection window from now and
// returns the new deadline.
func (s *sessionState) refreshRunnerActiveWindow() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerActiveUntil = time.Now().Add(s.activeWindow)
	return s.runnerActiveUntil
}

// abortPost re-enables renewal after a failed PostWork.
func (s *sessionState) abortPost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posting = false
}

// finishEvent commits one event's completion: the session goes idle and is
// awake (work just ran, which implies a resumed or fresh sandbox).
func (s *sessionState) finishEvent(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.inFlight = nil
	s.posted = false
	s.posting = false
	s.leaseLost = false
	s.lastDone = at
	s.suspended = false
}

// renewLoop keeps the leases of queued events and of the in-flight event
// (until the runner takes over heartbeating) alive. An event whose lease
// cannot be renewed is dropped or abandoned: CMA re-delivers it to a later
// poll, and running it with a dead lease could duplicate another worker's
// side effects.
func (s *sessionState) renewLoop(ctx context.Context, renewer LeaseRenewer, interval time.Duration) {
	if renewer == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		s.mu.Lock()
		targets := make([]WorkEvent, 0, len(s.pending)+1)
		for _, item := range s.pending {
			if !item.suspend {
				targets = append(targets, item.ev)
			}
		}
		if s.inFlight != nil && !s.posted && !s.posting && !s.leaseLost {
			targets = append(targets, *s.inFlight)
		}
		s.mu.Unlock()

		for _, ev := range targets {
			// The snapshot is only a hint: recheck eligibility and read the
			// current token under the same lock acquisition that claims the
			// renewal, so an event handed to the runner since the snapshot
			// is skipped instead of having its token rotated mid-handoff.
			tok0, ok := s.claimRenewal(ev.WorkID)
			if !ok {
				continue
			}
			tok, err := renewer.RenewLease(ctx, ev.WorkID, tok0)
			s.applyRenewal(ev.WorkID, tok, err)
			if err != nil {
				s.log.Info("Lease renewal failed; the item is left for re-delivery",
					"work_id", ev.WorkID)
			}
		}
	}
}

// claimRenewal atomically rechecks that workID is still renewable (queued,
// or in flight and not yet handed to the runner) and, if so, marks the
// renewal in progress and returns the event's current lease token.
func (s *sessionState) claimRenewal(workID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewingID != "" {
		return "", false
	}
	for i := range s.pending {
		if !s.pending[i].suspend && s.pending[i].ev.WorkID == workID {
			s.renewingID = workID
			return s.pending[i].ev.LastHeartbeat, true
		}
	}
	if s.inFlight != nil && s.inFlight.WorkID == workID &&
		!s.posted && !s.posting && !s.leaseLost {
		s.renewingID = workID
		return s.inFlight.LastHeartbeat, true
	}
	return "", false
}

// process runs one event with bounded retries.
func (s *sessionState) process(ctx context.Context, ev WorkEvent, retryBase time.Duration, attempts int) error {
	delay := retryBase
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = s.runOnce(ctx); err == nil {
			return nil
		}
		s.log.Error(err, "Work execution failed",
			"work_id", ev.WorkID, "attempt", attempt, "of", attempts)
		if attempt == attempts || s.leaseLostForInFlight() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	if s.turnPosted() {
		// The runner accepted (or may have accepted) the work but the
		// controller never saw it finish. Measure the protection window from
		// this abandonment, not from the post: a turn that ran longer than
		// the window would otherwise leave the pod unprotected while still
		// executing.
		until := s.refreshRunnerActiveWindow()
		s.log.Info("Work abandoned while the runner may still be executing it; lifecycle cleanup paused",
			"work_id", ev.WorkID, "blocked_until", until.UTC().Format(time.RFC3339))
	}
	return fmt.Errorf("work %s failed after %d attempts: %w", ev.WorkID, attempts, err)
}

// runOnce brings the sandbox up, hands the work to the runner with the
// latest lease token, and waits for the turn to finish.
func (s *sessionState) runOnce(ctx context.Context) error {
	if err := s.sb.EnsureReady(ctx); err != nil {
		return fmt.Errorf("ensuring sandbox ready: %w", err)
	}
	ev, err := s.takeForPost()
	if err != nil {
		// The lease died while the sandbox was starting; stop retrying and
		// let CMA re-deliver the item.
		return fmt.Errorf("work no longer leased: %w", err)
	}
	if err := s.sb.PostWork(ctx, ev); err != nil {
		if IsUncertainPost(err) {
			// The runner may have started the work before its response was
			// lost. Treat the lease as runner-owned in a single critical
			// section (posting=false, posted=true, window opens): renewal
			// must never re-enable between the failed post and this
			// transition, or it could rotate the token the runner is
			// heartbeating with.
			s.markPosted()
		} else {
			s.abortPost()
		}
		return fmt.Errorf("posting work: %w", err)
	}
	s.markPosted()
	s.log.Info("Work started", "work_id", ev.WorkID)
	if err := s.sb.WaitWorkFinished(ctx); err != nil {
		return fmt.Errorf("waiting for work to finish: %w", err)
	}
	s.confirmCompletion()
	s.log.Info("Work finished", "work_id", ev.WorkID)
	return nil
}

// leaseLostForInFlight reports whether the in-flight lease died, in which
// case retrying is pointless: CMA re-delivers the item to a later poll.
func (s *sessionState) leaseLostForInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaseLost
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
	return s.busy || s.hasWorkLocked() || s.reserved
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
// events (their leases lapse for re-delivery), so release cannot race a
// session back into activity.
func (s *sessionState) tryReserveRelease(now time.Time, releaseAfter time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || len(s.pending) > 0 || s.reserved || s.stopped {
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
