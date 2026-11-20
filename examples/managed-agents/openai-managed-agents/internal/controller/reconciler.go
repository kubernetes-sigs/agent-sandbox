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
// connected. It replaces the CMA bridge's WorkEvent: the OpenAI protocol has
// no work items, acks, or leases — a session either needs an environment
// connection or it does not.
type SessionEvent struct {
	SessionID     string
	EnvironmentID string
	// RemoteURL is a capability; never log it.
	RemoteURL string
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
	// Track registers a managed session that needs no connection right now,
	// so a sandbox it retained (e.g. across a controller restart) is still
	// swept by idle GC. It must not start work or allocate compute.
	Track(sessionID string)
	// RemoveSession drops all state for a session the platform reports
	// deleted and releases its sandbox.
	RemoveSession(sessionID string)
}

// Reconciler polls the Agents API for managed sessions whose
// required_actions include environment_connection and hands each confirmed
// one to the session manager. There is no ack in this protocol: idempotent
// consumption is the GET recheck (a stale list entry is dropped) plus the
// manager's dedup plus the runner's binding check.
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
		if !listed.NeedsConnection {
			// Connected sessions are tracked so the manager knows about the
			// sandboxes they hold after a restart; GC releases them once
			// idle. Tracking never starts an executor.
			r.disp.Track(listed.ID)
			continue
		}
		// Recheck with a fresh GET: the list may be stale, and only an
		// environment_connection action may start a sandbox — a
		// function_call action must not.
		fresh, err := r.api.GetSession(ctx, listed.ID)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				// The application deleted the session; deleting an Agents
				// API session does not reclaim self-hosted compute, so we
				// must release ours.
				r.log.Info("session deleted on platform, releasing sandbox", "session", listed.ID)
				r.disp.RemoveSession(listed.ID)
				continue
			}
			r.log.V(2).Info("recheck failed, skipping session", "session", listed.ID, "error", err.Error())
			continue
		}
		if !fresh.NeedsConnection || !fresh.Managed {
			// The metadata marker may have been removed since the list call;
			// never dispatch compute for a session that is not ours.
			r.log.V(4).Info("connection resolved or session unmanaged, skipping",
				"session", listed.ID)
			continue
		}
		ev := SessionEvent{
			SessionID:     fresh.ID,
			EnvironmentID: fresh.EnvironmentID,
			RemoteURL:     fresh.RemoteURL,
		}
		if ev.EnvironmentID == "" {
			ev.EnvironmentID = listed.EnvironmentID
		}
		if ev.RemoteURL == "" {
			ev.RemoteURL = listed.RemoteURL
		}
		if err := r.disp.Dispatch(ev); err != nil {
			if errors.Is(err, ErrAlreadyActive) {
				r.log.V(4).Info("session already active, deduped", "session", ev.SessionID)
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
