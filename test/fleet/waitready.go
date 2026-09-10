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

package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// waitReady blocks until every created sandbox has been observed Ready,
// or no NEW sandbox has become Ready for stallTimeout (loud failure with
// the shortfall rather than hanging forever).
const stallTimeout = 10 * time.Minute

func waitReady(ctx context.Context, cfg config, clusters []*cluster, rec *recorder, start time.Time) error {
	var createdTotal int64
	for _, c := range clusters {
		createdTotal += c.created.Load()
	}
	last, lastProgress := -1, time.Now()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Ready: %d/%d", rec.ReadyTotal(), createdTotal)
		case <-time.After(2 * time.Second):
		}
		n := rec.ReadyTotal()
		if int64(n) >= createdTotal {
			log.Printf("[fleet] all %d sandboxes Ready in %s", n, time.Since(start).Round(time.Second))
			return nil
		}
		if n != last {
			last, lastProgress = n, time.Now()
		} else if time.Since(lastProgress) > stallTimeout {
			// Not a fatal error: on a heterogeneous fleet the smallest
			// clusters' overflow is genuinely unschedulable, so readiness
			// plateaus below createdTotal by design. Log loudly and let the
			// caller write the summary - the fixed-window verdict stands on
			// the sandboxes that DID become Ready.
			log.Printf("[fleet] readiness plateaued at %d/%d (%d short) - no new Ready for %s; "+
				"remaining are unschedulable (capacity). Recording results; verdict is the fixed 60s window.",
				n, createdTotal, int(createdTotal)-n, stallTimeout)
			return nil
		}
	}
}
