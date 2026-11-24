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
	"fmt"
	"net/url"
	"strings"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/bma"
)

// InstructionsMarker tags the sessions this example manages. BMA has no
// metadata field, so the marker rides in the agent instructions.
const InstructionsMarker = "[managed-by: agent-sandbox-example]"

// awsAPI implements AgentAPI against the BMA preview REST surface.
type awsAPI struct {
	client *bma.Client
}

var _ AgentAPI = (*awsAPI)(nil)

// NewAWSAPI builds the platform client. The SigV4 credential stays in the
// controller process and must never be injected into sandboxes.
func NewAWSAPI(client *bma.Client) AgentAPI {
	return &awsAPI{client: client}
}

// bmaSession is the subset of the BMA session resource the bridge reads.
type bmaSession struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Agent  struct {
		Instructions string `json:"instructions"`
	} `json:"agent"`
	Environment struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"environment"`
}

type bmaSessionPage struct {
	Data    []bmaSession `json:"data"`
	HasMore bool         `json:"has_more"`
	LastID  string       `json:"last_id"`
}

func (a *awsAPI) ListManagedSessions(ctx context.Context) ([]SessionInfo, error) {
	var out []SessionInfo
	query := url.Values{"limit": {"100"}, "order": {"desc"}}
	for {
		var page bmaSessionPage
		if err := a.client.Do(ctx, "GET", "/agents/sessions", query, nil, &page); err != nil {
			return nil, fmt.Errorf("listing sessions: %w", err)
		}
		for _, s := range page.Data {
			if info := sessionInfoFrom(s); info.Managed {
				out = append(out, info)
			}
		}
		if !page.HasMore || page.LastID == "" {
			return out, nil
		}
		query.Set("after", page.LastID)
	}
}

func (a *awsAPI) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	var s bmaSession
	if err := a.client.Do(ctx, "GET", "/agents/sessions/"+url.PathEscape(id), nil, nil, &s); err != nil {
		if bma.IsNotFound(err) {
			return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return SessionInfo{}, fmt.Errorf("getting session: %w", err)
	}
	return sessionInfoFrom(s), nil
}

func sessionInfoFrom(s bmaSession) SessionInfo {
	info := SessionInfo{
		ID:         s.ID,
		Status:     s.Status,
		ActiveTurn: s.Status == "in_progress",
		Managed:    strings.Contains(s.Agent.Instructions, InstructionsMarker),
	}
	// Only self_hosted environments are this bridge's; an AgentCore Runtime
	// environment runs its compute inside AWS.
	if s.Environment.Type == "self_hosted" {
		info.EnvironmentID = s.Environment.ID
	}
	return info
}

// IsUnauthorized reports whether err is a 401/403 from BMA so the controller
// can fail fast on bad credentials instead of backing off forever.
func IsUnauthorized(err error) bool { return bma.IsUnauthorized(err) }
