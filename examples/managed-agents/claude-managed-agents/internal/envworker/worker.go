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

// Package envworker executes CMA work items in-process: it runs the session's
// tool calls with the Anthropic SDK's agent toolset while heartbeating the
// work-item lease, and force-stops the item on exit unless the lease was lost.
package envworker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"
)

const (
	heartbeatDefault    = 30 * time.Second
	heartbeatFloor      = 1 * time.Second
	noHeartbeatSentinel = "NO_HEARTBEAT"
	stopTimeout         = 10 * time.Second
)

type helperTag string

const helperEnvironmentsWorker helperTag = "environments-worker"

func withHelperTag(v helperTag) option.RequestOption {
	return option.WithHeader("x-stainless-helper", string(v))
}

// bearerReqOpts authenticates with the environment key instead of an API key.
func bearerReqOpts(environmentKey string) ([]option.RequestOption, error) {
	if environmentKey == "" {
		return nil, errors.New("environments: environment key is required")
	}
	return []option.RequestOption{
		option.WithHeaderDel("X-Api-Key"),
		option.WithAuthToken(environmentKey),
	}, nil
}

func helperReqOpts(environmentKey string) ([]option.RequestOption, error) {
	opts, err := bearerReqOpts(environmentKey)
	if err != nil {
		return nil, err
	}
	return append(opts, withHelperTag(helperEnvironmentsWorker)), nil
}

// EnvironmentWorkerOptions configures an EnvironmentWorker.
type EnvironmentWorkerOptions struct {
	Workdir string
	// AllowedRoots are directories the file tools may reach
	// (agenttoolset.AgentToolContext.AllowedRoots).
	AllowedRoots   []string
	Tools          []anthropic.BetaTool
	MaxIdle        *time.Duration
	RequestOptions []option.RequestOption
	Logger         *slog.Logger
}

// HandleItemOptions configures a work item execution.
type HandleItemOptions struct {
	WorkID                string
	EnvironmentID         string
	SessionID             string
	EnvironmentKey        string
	ExpectedLastHeartbeat string
}

// EnvironmentWorker executes session work items with tool dispatch and
// heartbeat management.
type EnvironmentWorker struct {
	client anthropic.Client
	opts   EnvironmentWorkerOptions
}

// NewEnvironmentWorker creates a new EnvironmentWorker instance.
func NewEnvironmentWorker(client anthropic.Client, opts EnvironmentWorkerOptions) *EnvironmentWorker {
	if opts.Workdir == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Workdir = wd
		} else {
			opts.Workdir = "."
		}
	}
	return &EnvironmentWorker{client: client, opts: opts}
}

// HandleItem executes a work item in-process: it runs the session tool runner
// against the Anthropic API, maintains the lease heartbeat in the background
// (continuing the handed-off lease when ExpectedLastHeartbeat is set), and
// force-stops the item on completion unless the lease was lost.
func (w *EnvironmentWorker) HandleItem(ctx context.Context, opts HandleItemOptions) error {
	workID := cmp.Or(opts.WorkID, os.Getenv("ANTHROPIC_WORK_ID"))
	environmentID := cmp.Or(opts.EnvironmentID, os.Getenv("ANTHROPIC_ENVIRONMENT_ID"))
	sessionID := cmp.Or(opts.SessionID, os.Getenv("ANTHROPIC_SESSION_ID"))
	environmentKey := cmp.Or(opts.EnvironmentKey, os.Getenv("ANTHROPIC_ENVIRONMENT_KEY"))
	expectedLastHeartbeat := cmp.Or(opts.ExpectedLastHeartbeat, os.Getenv("ANTHROPIC_WORK_LAST_HEARTBEAT"))

	for _, req := range []struct{ name, val, env string }{
		{"work_id", workID, "ANTHROPIC_WORK_ID"},
		{"environment_id", environmentID, "ANTHROPIC_ENVIRONMENT_ID"},
		{"session_id", sessionID, "ANTHROPIC_SESSION_ID"},
		{"environment_key", environmentKey, "ANTHROPIC_ENVIRONMENT_KEY"},
	} {
		if req.val == "" {
			return fmt.Errorf("EnvironmentWorker.HandleItem: %s is required — pass it in HandleItemOptions or set %s", req.name, req.env)
		}
	}

	work := &anthropic.BetaSelfHostedWork{
		ID:            workID,
		EnvironmentID: environmentID,
		Data: anthropic.BetaSessionWorkData{
			ID: sessionID,
		},
	}

	log := w.opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("work_id", work.ID), slog.String("session_id", sessionID))

	hbStopOpts, err := helperReqOpts(environmentKey)
	if err != nil {
		return err
	}
	hbStopOpts = append(append([]option.RequestOption{}, w.opts.RequestOptions...), hbStopOpts...)

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	hbDone := make(chan struct{})
	firstBeat := make(chan bool, 1)
	var leaseLost bool
	go func() {
		defer close(hbDone)
		leaseLost = runHeartbeat(sessCtx, sessCancel, w.client, work, hbStopOpts, expectedLastHeartbeat, log, firstBeat)
	}()

	// Run tools only after the lease is confirmed: a 412 on the first
	// heartbeat means another worker owns the item.
	if !<-firstBeat {
		<-hbDone
		log.Info("lease not confirmed by the first heartbeat; skipping tool execution")
		return nil
	}

	env := &agenttoolset.AgentToolContext{
		Workdir:      w.opts.Workdir,
		AllowedRoots: w.opts.AllowedRoots,
	}
	if err := env.SetupSkills(sessCtx, w.client, sessionID, hbStopOpts...); err != nil {
		log.Warn("skill setup failed", slog.Any("error", err))
	}
	defer func() {
		if err := env.Cleanup(); err != nil {
			log.Warn("skill cleanup failed", slog.Any("error", err))
		}
	}()

	tools := w.opts.Tools
	if tools == nil {
		tools = agenttoolset.BetaAgentToolset20260401(env)
		defer agenttoolset.CloseAll(tools)
	}

	runnerBearerOpts, err := bearerReqOpts(environmentKey)
	if err != nil {
		return err
	}
	runnerReqOpts := append(append([]option.RequestOption{}, w.opts.RequestOptions...), runnerBearerOpts...)

	runner := w.client.Beta.Sessions.Events.NewToolRunner(sessCtx, sessionID, anthropic.SessionToolRunnerOptions{
		Tools:          tools,
		MaxIdle:        w.opts.MaxIdle,
		Logger:         log,
		RequestOptions: runnerReqOpts,
	})
	for runner.Next() {
		call := runner.Current()
		log.Info("dispatched tool",
			slog.String("tool", call.Name),
			slog.Bool("is_error", call.IsError),
			slog.Bool("posted", call.Posted),
		)
	}
	var runErr error
	if err := runner.Err(); err != nil &&
		!errors.Is(err, anthropic.ErrSessionTerminated) &&
		!errors.Is(err, anthropic.ErrIdleTimeout) {
		log.Warn("session tool runner exited with error", slog.Any("error", err))
		runErr = err
	}
	_ = runner.Close()

	sessCancel()
	<-hbDone

	if leaseLost {
		log.Info("lease lost; released without stopping it")
		return runErr
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := stopWork(stopCtx, w.client, work.ID,
		anthropic.BetaEnvironmentWorkStopParams{
			EnvironmentID: work.EnvironmentID,
			BetaSelfHostedWorkStopRequest: anthropic.BetaSelfHostedWorkStopRequestParam{
				Force: param.NewOpt(true),
			},
		},
		hbStopOpts...,
	); err != nil && !isStatus(err, 409) {
		log.Warn("force-stop on exit failed", slog.Any("error", err))
	}

	return runErr
}

// beatResult classifies one heartbeat attempt.
type beatResult int

const (
	// beatOK means the lease was confirmed or renewed.
	beatOK beatResult = iota
	// beatRetry means a transient failure; the loop beats again.
	beatRetry
	// beatStop ends the loop (clean shutdown, permanent error, or loss).
	beatStop
)

// runHeartbeat keeps the work-item lease alive until the context ends, the
// API reports the item stopping/stopped, the lease is lost (412, expiry, or
// a beat that cannot finish within the remaining lease lifetime), or a
// permanent 4xx error occurs. It returns whether the lease was lost to
// another claimant. Every stop path calls cancel() so tool execution ends
// with the heartbeat, and reports exactly once on firstBeat (buffered):
// true only after a heartbeat response actually confirmed the lease, false
// if the loop stops before that. The caller must see true before running
// any tool.
func runHeartbeat(ctx context.Context, cancel context.CancelFunc, client anthropic.Client, work *anthropic.BetaSelfHostedWork, reqOpts []option.RequestOption, expectedLastHeartbeat string, log *slog.Logger, firstBeat chan<- bool) (lost bool) {
	interval := heartbeatDefault
	ttl := heartbeatDefault
	last := cmp.Or(expectedLastHeartbeat, noHeartbeatSentinel)
	lastSuccess := time.Now()

	confirmed := false
	sent := false
	// report sends the firstBeat verdict exactly once. Every exit path below
	// goes through it, so HandleItem can never hang waiting for a verdict.
	report := func(ok bool) {
		if !sent && firstBeat != nil {
			firstBeat <- ok
		}
		sent = true
	}

	beat := func() beatResult {
		// Bound the whole call, SDK retries included, by the remaining
		// lease: a beat that cannot finish in time cannot renew it.
		remaining := ttl - time.Since(lastSuccess)
		if remaining < heartbeatFloor {
			log.Error("lease expired before the next heartbeat could start",
				slog.Duration("since_last_success", time.Since(lastSuccess)),
				slog.Duration("ttl", ttl))
			lost = true
			return beatStop
		}
		beatCtx, cancelBeat := context.WithTimeout(ctx, remaining)
		defer cancelBeat()
		beatOpts := append(reqOpts[:len(reqOpts):len(reqOpts)], option.WithRequestTimeout(remaining))
		// The server timestamps the renewal when it processes the request,
		// not when the response arrives. Measuring from the send time keeps
		// the local lease clock conservative: a slow response cannot extend
		// the computed deadline past the server-side expiry.
		sentAt := time.Now()
		resp, err := client.Beta.Environments.Work.Heartbeat(
			beatCtx,
			work.ID,
			anthropic.BetaEnvironmentWorkHeartbeatParams{
				EnvironmentID:         work.EnvironmentID,
				ExpectedLastHeartbeat: param.NewOpt(last),
			},
			beatOpts...,
		)
		if err != nil {
			if ctx.Err() != nil {
				return beatStop
			}
			if isStatus(err, 412) {
				log.Warn("heartbeat precondition failed; lease reclaimed by another worker", errAttrs(err)...)
				lost = true
				return beatStop
			}
			if isFatal4xx(err) {
				log.Error("permanent heartbeat failure", errAttrs(err)...)
				return beatStop
			}
			if beatCtx.Err() != nil {
				log.Error("heartbeat did not complete within the remaining lease lifetime", errAttrs(err)...)
				lost = true
				return beatStop
			}
			log.Warn("transient heartbeat failure", errAttrs(err)...)
			return beatRetry
		}
		last = resp.LastHeartbeat
		lastSuccess = sentAt
		if resp.TTLSeconds > 0 {
			ttl = max(time.Duration(resp.TTLSeconds)*time.Second, heartbeatFloor)
			interval = clampDur(ttl/2, heartbeatFloor, heartbeatDefault)
		}
		switch resp.State {
		case anthropic.BetaSelfHostedWorkHeartbeatResponseStateStopping,
			anthropic.BetaSelfHostedWorkHeartbeatResponseStateStopped:
			log.Info("heartbeat reports shutdown", slog.String("state", string(resp.State)))
			return beatStop
		}
		if !resp.LeaseExtended {
			log.Warn("lease not extended; shutting down")
			return beatStop
		}
		return beatOK
	}

	for {
		switch res := beat(); res {
		case beatOK:
			if !confirmed {
				confirmed = true
				report(true)
			}
		case beatStop:
			report(false)
			cancel()
			return lost
		case beatRetry:
			// Unconfirmed leases stay unconfirmed: the loop beats again
			// without letting HandleItem start tools.
		}

		// Cap the wait by the remaining lease so a transient failure cannot
		// push the next attempt past expiry.
		wait := min(interval, ttl-time.Since(lastSuccess))
		if wait <= 0 {
			log.Error("lease expired while waiting to heartbeat again",
				slog.Duration("since_last_success", time.Since(lastSuccess)),
				slog.Duration("ttl", ttl))
			lost = true
			report(false)
			cancel()
			return lost
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			report(false)
			return lost
		case <-t.C:
		}
	}
}

// errAttrs sanitizes heartbeat errors for logging: SDK error strings embed
// the request URL, whose query carries the lease token. Only the status code
// and request ID are kept; other errors reduce to their type.
func errAttrs(err error) []any {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return []any{
			slog.Int("status", apiErr.StatusCode),
			slog.String("request_id", apiErr.RequestID),
		}
	}
	return []any{slog.String("error_type", fmt.Sprintf("%T", err))}
}

func clampDur(v, lo, hi time.Duration) time.Duration {
	return max(lo, min(hi, v))
}

func stopWork(ctx context.Context, client anthropic.Client, workID string, params anthropic.BetaEnvironmentWorkStopParams, opts ...option.RequestOption) error {
	var raw *http.Response
	opts = append(opts, option.WithResponseBodyInto(&raw))
	_, err := client.Beta.Environments.Work.Stop(ctx, workID, params, opts...)
	if raw != nil && raw.Body != nil {
		_ = raw.Body.Close()
	}
	return err
}

func isStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func isFatal4xx(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	c := apiErr.StatusCode
	return c >= 400 && c < 500 && c != 408 && c != 409 && c != 429
}
