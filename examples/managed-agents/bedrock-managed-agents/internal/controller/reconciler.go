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
	"time"

	"github.com/go-logr/logr"
)

// SessionEvent tells the session manager that a session needs its executor
// attached. BMA has no work items, acks, or leases: the reconciler
// re-asserts the attachment until the manager confirms it.
type SessionEvent struct {
	SessionID     string
	EnvironmentID string
	// Status snapshots the platform session state at dispatch time
	// ("idle"/"in_progress"). "in_progress" is the resume trigger: only an
	// active turn may wake a suspended sandbox.
	Status string
}

// ErrAlreadyActive is returned by a Dispatcher when the session already has
// an in-flight event. The reconciler treats it as successful idempotent
// consumption, not an error.
var ErrAlreadyActive = errors.New("session already active")

// Dispatcher is the session-manager surface the reconciler needs. Declared
// here so the reconciler is testable ahead of the session manager port.
type Dispatcher interface {
	// Dispatch enqueues an event for the session, starting its serialized
	// goroutine if needed. Returns ErrAlreadyActive when deduplicated.
	Dispatch(ev SessionEvent) error
	// Track registers a managed session that needs no executor right now
	// (e.g. a terminally failed one), so a sandbox it retained (across a
	// controller restart) is still swept by idle GC. It must not start work
	// or allocate compute.
	Track(sessionID string)
	// RemoveSession drops all state for a session the platform reports
	// deleted and releases its sandbox.
	RemoveSession(sessionID string)
}

// Reconciler polls BMA for managed sessions and (re-)asserts each one's
// executor attachment. BMA publishes no connection state, so every managed
// session is re-delivered each poll; the GET recheck, the manager's dedup,
// and the runner's already_running answer make that idempotent.
type Reconciler struct {
	api      AgentAPI
	disp     Dispatcher
	interval time.Duration
	log      logr.Logger
}

// NewReconciler builds the poll loop. interval is the poll period; Run adds
// exponential backoff on list failures.
func NewReconciler(api AgentAPI, disp Dispatcher, interval time.Duration, log logr.Logger) *Reconciler {
	return &Reconciler{api: api, disp: disp, interval: interval, log: log}
}

// PollOnce runs one reconciliation pass. It returns an error only when the
// session list itself failed (so Run can back off); per-session problems are
// logged and skipped.
func (r *Reconciler) PollOnce(ctx context.Context) error {
	sessions, err := r.api.ListManagedSessions(ctx)
	if err != nil {
		return fmt.Errorf("listing managed sessions: %w", err)
	}
	for _, listed := range sessions {
		// Recheck with a fresh GET: the list may be stale, and the marker
		// check must run against current state before compute is allocated.
		fresh, err := r.api.GetSession(ctx, listed.ID)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				// The application deleted the session; deleting a BMA session
				// does not reclaim self-hosted compute, so we must release
				// ours.
				r.log.Info("session deleted on platform, releasing sandbox", "session", listed.ID)
				r.disp.RemoveSession(listed.ID)
				continue
			}
			r.log.V(2).Info("recheck failed, skipping session", "session", listed.ID, "error", err.Error())
			continue
		}
		if !fresh.Managed {
			// The marker may have been removed since the list call; never
			// dispatch compute for a session that is not ours.
			r.log.V(4).Info("session unmanaged, skipping", "session", listed.ID)
			continue
		}
		if fresh.Status == "failed" {
			// Terminal platform state: track so GC releases any sandbox, but
			// never attach an executor to a failed session.
			r.disp.Track(fresh.ID)
			continue
		}
		if fresh.EnvironmentID == "" {
			// Not a self_hosted environment (or the platform omitted its
			// ID): this bridge has nothing to attach.
			r.log.V(2).Info("session has no self-hosted environment, skipping",
				"session", listed.ID)
			continue
		}
		ev := SessionEvent{
			SessionID:     fresh.ID,
			EnvironmentID: fresh.EnvironmentID,
			Status:        fresh.Status,
		}
		if err := r.disp.Dispatch(ev); err != nil {
			if errors.Is(err, ErrAlreadyActive) {
				r.log.V(4).Info("session already active or attached, deduped", "session", ev.SessionID)
				continue
			}
			r.log.Error(err, "dispatch failed", "session", ev.SessionID)
		}
	}
	return nil
}

// Run polls until ctx is done, backing off exponentially (interval → 30s
// cap) while the list call fails and resetting on success.
func (r *Reconciler) Run(ctx context.Context) error {
	if r.interval <= 0 {
		return fmt.Errorf("invalid --poll-interval %s: must be positive", r.interval)
	}
	backoff := r.interval
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.PollOnce(ctx); err != nil {
			r.log.Error(err, "poll failed, backing off", "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = r.interval
		// Drop a tick that fired during a long poll so the next pass still
		// waits a full interval.
		select {
		case <-ticker.C:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
