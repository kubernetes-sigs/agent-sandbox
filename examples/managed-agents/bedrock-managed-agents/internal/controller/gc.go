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
	"time"

	"github.com/go-logr/logr"
)

// GC suspends session sandboxes idle for at least the suspend threshold and
// releases the claim once the idle time passes the release threshold.
// Before any lifecycle action it asks the platform whether the session is
// still running a turn, so suspension never kills a pod mid-turn.
type GC struct {
	mgr          *SessionManager
	api          AgentAPI
	idleSuspend  time.Duration
	releaseAfter time.Duration
	interval     time.Duration
	log          logr.Logger
}

// NewGC creates the GC loop. interval is how often Sweep runs. api may be
// nil in tests, which disables the platform-side veto.
func NewGC(mgr *SessionManager, api AgentAPI, idleSuspend, releaseAfter, interval time.Duration, log logr.Logger) *GC {
	return &GC{
		mgr:          mgr,
		api:          api,
		idleSuspend:  idleSuspend,
		releaseAfter: releaseAfter,
		interval:     interval,
		log:          log,
	}
}

// Run sweeps on the interval until ctx is cancelled.
func (g *GC) Run(ctx context.Context) error {
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			g.Sweep(ctx)
		}
	}
}

// Sweep makes one pass over all sessions. Suspension is queued into the
// session's own FIFO loop, so it can never terminate a pod with a turn in
// flight; the loop rechecks idleness and the platform state when it
// processes the request. Release deletes session state, so the session is
// reserved first — atomically rechecking busy state and idle duration —
// which makes concurrent dispatches fail (the platform re-delivers the
// action) instead of racing the cleanup.
func (g *GC) Sweep(ctx context.Context) {
	now := time.Now()
	for _, s := range g.mgr.list() {
		if s.isReleased() {
			// No sandbox left to manage; the reconciler drops the entry
			// when the platform reports the session deleted.
			continue
		}
		idle := s.idleSince(now)
		if idle < g.idleSuspend {
			continue
		}

		// Platform-side veto before any lifecycle action. A session the
		// application deleted releases immediately: deleting a BMA session
		// does not reclaim self-hosted compute, so the bridge must.
		if g.api != nil {
			info, err := g.api.GetSession(ctx, s.id)
			switch {
			case errors.Is(err, ErrSessionNotFound):
				g.releaseNow(ctx, s, now, idle)
				continue
			case err != nil:
				// Conservative: unknown platform state keeps the sandbox up.
				g.log.V(4).Info("GC skipped session: platform check failed",
					"session_id", s.id, "error", err.Error())
				continue
			case info.ActiveTurn:
				g.log.V(4).Info("GC skipped session: platform reports it active",
					"session_id", s.id)
				continue
			}
		}

		if idle >= g.releaseAfter {
			g.releaseNow(ctx, s, now, idle)
			continue
		}
		// requestSuspend itself rejects sessions that are already suspended.
		s.requestSuspend(g.idleSuspend)
	}
}

// releaseNow reserves the session, releases its sandbox, and marks it
// released so idle re-deliveries cannot claim a replacement. threshold 0
// skips the release-after idle recheck (platform-deleted sessions).
func (g *GC) releaseNow(ctx context.Context, s *sessionState, now time.Time, idle time.Duration) {
	threshold := g.releaseAfter
	if idle < threshold {
		// Platform-deleted session: release as soon as it is safe.
		threshold = 0
	}
	if !s.tryReserveRelease(now, threshold) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	err := s.sb.Release(rctx)
	cancel()
	if err != nil {
		s.unreserve()
		g.log.Error(err, "Releasing session sandbox failed", "session_id", s.id)
		return
	}
	s.markReleased()
	g.log.Info("Released session sandbox", "session_id", s.id, "idle", idle.Round(time.Second))
}
