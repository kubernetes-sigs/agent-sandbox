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
	"time"

	"github.com/go-logr/logr"
)

// lifecycleTimeout bounds a single Suspend or Release call so one stuck
// sandbox cannot stall the sweep.
const lifecycleTimeout = 60 * time.Second

// GC suspends session sandboxes idle for at least the suspend threshold and
// releases the claim once the idle time passes the release threshold. It
// mirrors the upstream controller's garbage-collection loop, with the SDK's
// Suspend replacing the Substrate snapshot-based suspend.
type GC struct {
	mgr          *SessionManager
	idleSuspend  time.Duration
	releaseAfter time.Duration
	interval     time.Duration
	log          logr.Logger
}

// NewGC creates the GC loop. interval is how often Sweep runs.
func NewGC(mgr *SessionManager, idleSuspend, releaseAfter, interval time.Duration, log logr.Logger) *GC {
	return &GC{
		mgr:          mgr,
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
// session's own FIFO loop, so it can never terminate a pod with work in
// flight; the loop rechecks idleness when it processes the request. Release
// deletes session state, so the session is reserved first — atomically
// rechecking busy state and idle duration — which makes concurrent
// dispatches fail (their leases lapse for re-delivery) instead of racing
// the cleanup.
func (g *GC) Sweep(ctx context.Context) {
	now := time.Now()
	for _, s := range g.mgr.list() {
		idle := s.idleSince(now)

		if idle >= g.releaseAfter {
			if !s.tryReserveRelease(now, g.releaseAfter) {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
			err := s.sb.Release(rctx)
			cancel()
			if err != nil {
				s.unreserve()
				g.log.Error(err, "Releasing idle session sandbox failed", "session_id", s.id)
				continue
			}
			g.mgr.remove(s.id)
			g.log.Info("Released idle session sandbox", "session_id", s.id, "idle", idle.Round(time.Second))
			continue
		}

		if idle >= g.idleSuspend && !s.suspendedState() {
			s.requestSuspend(g.idleSuspend)
		}
	}
}
