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

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Metadata marker: the reconciler only ever manages sessions created with
// this pair, so an application's other Agents API sessions are untouched.
const (
	MetadataManagedByKey   = "managed-by"
	MetadataManagedByValue = "agent-sandbox-example"
)

// openaiAPI implements AgentAPI against openai-go's Beta.Agents surface
// (verified against openai-go v3.105.0; see the scaffold commit message for
// the recorded signatures).
type openaiAPI struct {
	client openai.Client
}

var _ AgentAPI = (*openaiAPI)(nil)

// NewOpenAIAPI builds the platform client. apiKey is the application-side
// credential (api.agents.read / api.agents.write); it stays in the
// controller process and must never be injected into sandboxes.
func NewOpenAIAPI(apiKey string) AgentAPI {
	return &openaiAPI{client: openai.NewClient(option.WithAPIKey(apiKey))}
}

func (a *openaiAPI) ListManagedSessions(ctx context.Context) ([]SessionInfo, error) {
	md := map[string]string{MetadataManagedByKey: MetadataManagedByValue}
	var out []SessionInfo
	pager := a.client.Beta.Agents.Sessions.ListAutoPaging(ctx, openai.BetaAgentSessionListParams{
		Metadata: &md,
		Limit:    openai.Int(100),
	})
	for pager.Next() {
		out = append(out, sessionInfoFrom(pager.Current()))
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return out, nil
}

func (a *openaiAPI) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	sess, err := a.client.Beta.Agents.Sessions.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return SessionInfo{}, fmt.Errorf("getting session: %w", err)
	}
	info := sessionInfoFrom(*sess)

	// Fresh turn state: the session status alone does not distinguish a
	// completed turn from a failed one, and connection health must stay a
	// separate record from work completion.
	// Default order is desc, so the first root turn seen is the latest. A
	// page can hold only subagent turns; keep paging until a root turn shows
	// up or the scan bound is reached.
	const maxTurnScan = 50 // 5 pages of 10
	pager := a.client.Beta.Agents.Sessions.Turns.ListAutoPaging(ctx, id, openai.BetaAgentSessionTurnListParams{
		Limit: openai.Int(10),
	})
	scanned := 0
	for pager.Next() {
		t := pager.Current()
		if t.SubagentID == "" {
			info.LatestTurnState = string(t.Status)
			switch t.Status {
			case openai.TurnStatusQueued, openai.TurnStatusInProgress, openai.TurnStatusWaiting:
				info.ActiveTurn = true
			}
			break
		}
		scanned++
		if scanned >= maxTurnScan {
			break
		}
	}
	if err := pager.Err(); err != nil && !isNotFound(err) {
		return SessionInfo{}, fmt.Errorf("listing turns: %w", err)
	}
	return info, nil
}

func sessionInfoFrom(sess openai.AgentSession) SessionInfo {
	info := SessionInfo{
		ID:         sess.ID,
		Managed:    sess.Metadata[MetadataManagedByKey] == MetadataManagedByValue,
		ActiveTurn: sess.Status == openai.AgentSessionStatusInProgress,
	}
	if sess.Environment.Type == "self_hosted" {
		info.EnvironmentID = sess.Environment.ID
		info.RemoteURL = sess.Environment.RemoteURL
	}
	for _, ra := range sess.RequiredActions {
		if ra.Type == "environment_connection" {
			info.NeedsConnection = true
			if ra.EnvironmentID != "" {
				info.EnvironmentID = ra.EnvironmentID
			}
		}
	}
	return info
}

func isNotFound(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 404
	}
	return false
}

// IsUnauthorized reports whether err is a 401 from the platform, so the
// controller can fail fast on a bad application key instead of silently
// backing off forever.
func IsUnauthorized(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 401
	}
	return false
}
