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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/go-logr/logr"
)

// ErrNoWork is returned when a poll attempt yields no available work item.
var ErrNoWork = errors.New("no work available")

const (
	defaultPollInterval = 1 * time.Second

	// DefaultHandoffTTLSeconds is the lease the poller establishes before
	// handing a work item to the session manager: the time the controller
	// has to make the session's sandbox ready and start the worker before
	// the lease lapses and the item is requeued.
	DefaultHandoffTTLSeconds int64 = 120

	// pollReclaimOlderThanMs is the server default for reclaim_older_than_ms.
	// The poller acks every item right after the poll, so the window only
	// needs to cover the poll-to-ack round trip; it is stated explicitly so
	// that dependence is visible.
	pollReclaimOlderThanMs int64 = 5000

	// noHeartbeat is the expected_last_heartbeat value that claims a lease
	// nobody has heartbeated yet.
	noHeartbeat = "NO_HEARTBEAT"
)

// PollerConfig configures the Poller client and lifecycle.
type PollerConfig struct {
	Client            *anthropic.Client
	BaseURL           string
	APIKey            string
	EnvironmentID     string
	EnvironmentKey    string
	PollInterval      time.Duration
	BlockMs           int64
	HandoffTTLSeconds int64
	HTTPClient        *http.Client
	Logger            logr.Logger
}

// Defaults populates default configuration values for unset fields.
func (c *PollerConfig) Defaults() {
	if c.EnvironmentID == "" {
		c.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	}
	if c.EnvironmentKey == "" {
		c.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if c.BaseURL == "" {
		c.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.HandoffTTLSeconds <= 0 {
		c.HandoffTTLSeconds = DefaultHandoffTTLSeconds
	}
}

// Poller polls Anthropic for self-hosted environment work items and
// establishes each item's lease before handing it to the session manager.
type Poller struct {
	client *anthropic.Client
	cfg    PollerConfig
}

// NewPoller creates a new Poller using the Anthropic Go SDK.
func NewPoller(cfg PollerConfig) (*Poller, error) {
	cfg.Defaults()
	if cfg.EnvironmentID == "" {
		return nil, errors.New("environment_id is required (set ANTHROPIC_ENVIRONMENT_ID)")
	}

	client := cfg.Client
	if client == nil {
		opts := []option.RequestOption{
			option.WithoutEnvironmentDefaults(),
		}
		if cfg.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(cfg.BaseURL))
		}
		if cfg.APIKey != "" {
			opts = append(opts, option.WithAPIKey(cfg.APIKey))
		}
		if cfg.EnvironmentKey != "" {
			opts = append(opts, option.WithAuthToken(cfg.EnvironmentKey))
		}
		if cfg.HTTPClient != nil {
			opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
		}
		c := anthropic.NewClient(opts...)
		client = &c
	}
	return &Poller{client: client, cfg: cfg}, nil
}

// PollOnce claims the next available work item for the configured
// environment. If no work is available, it returns ErrNoWork.
func (p *Poller) PollOnce(ctx context.Context) (*anthropic.BetaSelfHostedWork, error) {
	params := anthropic.BetaEnvironmentWorkPollParams{
		ReclaimOlderThanMs: param.NewOpt(pollReclaimOlderThanMs),
	}
	if p.cfg.BlockMs > 0 {
		params.BlockMs = param.NewOpt(p.cfg.BlockMs)
	}

	res, err := p.client.Beta.Environments.Work.Poll(ctx, p.cfg.EnvironmentID, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusNotFound, http.StatusNoContent, http.StatusRequestTimeout:
				return nil, ErrNoWork
			}
		}
		return nil, fmt.Errorf("polling work for environment %s: %w", p.cfg.EnvironmentID, err)
	}
	if res == nil || res.ID == "" {
		return nil, ErrNoWork
	}
	return res, nil
}

// Complete marks a claimed work item as completed/stopped.
func (p *Poller) Complete(ctx context.Context, workID string, force bool) (*anthropic.BetaSelfHostedWork, error) {
	if workID == "" {
		return nil, errors.New("work_id is required")
	}
	return p.client.Beta.Environments.Work.Stop(ctx, workID, anthropic.BetaEnvironmentWorkStopParams{
		EnvironmentID: p.cfg.EnvironmentID,
		BetaSelfHostedWorkStopRequest: anthropic.BetaSelfHostedWorkStopRequestParam{
			Force: param.NewOpt(force),
		},
	})
}

// Lease acknowledges a polled work item and sends its first heartbeat, so
// the item is no longer reclaimable and holds a lease of HandoffTTLSeconds.
// It returns the last_heartbeat value the heartbeat response carried: the
// worker that continues the lease must quote it as expected_last_heartbeat.
func (p *Poller) Lease(ctx context.Context, item *anthropic.BetaSelfHostedWork) (string, error) {
	if item == nil || item.ID == "" {
		return "", errors.New("work_id is required")
	}
	if _, err := p.client.Beta.Environments.Work.Ack(ctx, item.ID, anthropic.BetaEnvironmentWorkAckParams{
		EnvironmentID: p.cfg.EnvironmentID,
	}); err != nil {
		return "", fmt.Errorf("acking work %s: %w", item.ID, err)
	}
	res, err := p.client.Beta.Environments.Work.Heartbeat(ctx, item.ID, anthropic.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID:         p.cfg.EnvironmentID,
		DesiredTTLSeconds:     param.NewOpt(p.cfg.HandoffTTLSeconds),
		ExpectedLastHeartbeat: param.NewOpt(noHeartbeat),
	})
	if err == nil && (!res.LeaseExtended || res.LastHeartbeat == "") {
		err = fmt.Errorf("lease not established (state %q)", res.State)
	}
	if err != nil {
		// An acked item that is never heartbeated is not requeued by the
		// server, so release it rather than leave the session waiting on it.
		p.release(item.ID)
		return "", fmt.Errorf("first heartbeat for work %s: %w", item.ID, err)
	}
	return res.LastHeartbeat, nil
}

// RenewLease extends the lease of a queued work item and returns the new
// token to quote in the next renewal or hand-off. Errors are sanitized: the
// SDK error string embeds the request URL, whose query carries the lease
// token.
func (p *Poller) RenewLease(ctx context.Context, workID, expectedLastHeartbeat string) (string, error) {
	res, err := p.client.Beta.Environments.Work.Heartbeat(ctx, workID, anthropic.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID:         p.cfg.EnvironmentID,
		DesiredTTLSeconds:     param.NewOpt(p.cfg.HandoffTTLSeconds),
		ExpectedLastHeartbeat: param.NewOpt(expectedLastHeartbeat),
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("renewing lease for work %s: HTTP %d", workID, apiErr.StatusCode)
		}
		return "", fmt.Errorf("renewing lease for work %s: %T", workID, err)
	}
	if !res.LeaseExtended || res.LastHeartbeat == "" {
		return "", fmt.Errorf("lease not extended for work %s (state %q)", workID, res.State)
	}
	return res.LastHeartbeat, nil
}

// release best-effort force-stops a work item the poller acked but cannot
// hand over. It uses its own context so a cancelled poll loop does not skip
// it.
func (p *Poller) release(workID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, workID, true); err != nil {
		p.cfg.Logger.Error(err, "Releasing un-leased work item failed", "work_id", workID)
	}
}

// Claim is a work item the poller has polled, acked, and heartbeated once.
type Claim struct {
	// Work is the polled item. Work.Data.ID carries the session ID.
	Work *anthropic.BetaSelfHostedWork
	// LastHeartbeat is the token the worker must send as its first
	// expected_last_heartbeat to continue the lease.
	LastHeartbeat string
}

// Handler is invoked for each claimed work item.
type Handler func(ctx context.Context, claim Claim) error

// Poll runs the continuous polling loop. For each claimed item it invokes
// handler and returns to polling immediately — it never waits for the
// sandbox to wake up or execute.
func (p *Poller) Poll(ctx context.Context, handler Handler) error {
	p.cfg.Logger.Info("Starting CMA work polling loop",
		"environment_id", p.cfg.EnvironmentID,
		"poll_interval", p.cfg.PollInterval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		item, err := p.PollOnce(ctx)
		if err != nil {
			if !errors.Is(err, ErrNoWork) {
				p.cfg.Logger.Error(err, "Polling work failed, backing off")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.cfg.PollInterval):
				continue
			}
		}

		// Ack and lease before enqueueing. An item whose ack failed is left
		// alone and is requeued by the server after the reclaim window.
		lastHeartbeat, err := p.Lease(ctx, item)
		if err != nil {
			p.cfg.Logger.Error(err, "Leasing work item failed, skipping",
				"work_id", item.ID, "session_id", item.Data.ID)
			continue
		}

		p.cfg.Logger.V(4).Info("Claimed work item",
			"work_id", item.ID, "session_id", item.Data.ID,
			"handoff_ttl_seconds", p.cfg.HandoffTTLSeconds)

		if err := handler(ctx, Claim{Work: item, LastHeartbeat: lastHeartbeat}); err != nil {
			p.cfg.Logger.Error(err, "Work item dispatch failed", "work_id", item.ID)
			continue
		}
	}
}
