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
	"sort"
	"sync"
)

// ErrSessionNotFound is returned by GetSession when the platform reports the
// session gone (HTTP 404). The reconciler treats it as "the application
// deleted the session" and releases the sandbox: deleting an Agents API
// session does not reclaim self-hosted compute, so the bridge must.
var ErrSessionNotFound = errors.New("session not found")

// SessionInfo is the bridge's view of one Agents API session. It flattens
// the fields the controller needs out of the beta SDK types so that the
// rest of the bridge never imports the SDK.
type SessionInfo struct {
	ID            string
	EnvironmentID string
	// RemoteURL is a capability (anyone holding it can connect an executor
	// to this session's environment). It flows controller → runner and must
	// never be logged.
	RemoteURL string
	// NeedsConnection reports a pending environment_connection required
	// action: the session is waiting for an executor to connect.
	NeedsConnection bool
	// ActiveTurn reports a turn in flight (session status in_progress, or a
	// root turn queued/in_progress/waiting).
	ActiveTurn bool
	// LatestTurnState is the status of the most recent root turn:
	// "", "queued", "in_progress", "waiting", "completed", "failed",
	// "cancelled".
	LatestTurnState string
	// Managed marks sessions carrying this example's metadata marker;
	// only managed sessions are ever dispatched.
	Managed bool
}

// AgentAPI is the narrow platform interface the bridge depends on.
// api_openai.go implements it against openai-go's Beta.Agents surface.
type AgentAPI interface {
	// ListManagedSessions returns sessions carrying the example's metadata
	// marker. The list may be stale: callers must recheck with GetSession
	// before acting on NeedsConnection.
	ListManagedSessions(ctx context.Context) ([]SessionInfo, error)
	// GetSession returns fresh state for one session, or an error wrapping
	// ErrSessionNotFound when the platform reports it deleted.
	GetSession(ctx context.Context, id string) (SessionInfo, error)
}

// FakeAPI is an in-memory AgentAPI for unit tests. Sessions holds the
// authoritative (fresh) state; ListOverride, when non-nil, is what
// ListManagedSessions returns instead — tests use it to model a stale list
// against fresh Get results.
type FakeAPI struct {
	mu           sync.Mutex
	Sessions     map[string]SessionInfo
	ListOverride []SessionInfo
	ListErr      error
	GetErr       error
	ListCalls    int
	GetCalls     int
}

var _ AgentAPI = (*FakeAPI)(nil)

func (f *FakeAPI) ListManagedSessions(_ context.Context) ([]SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListCalls++
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	src := f.ListOverride
	if src == nil {
		src = make([]SessionInfo, 0, len(f.Sessions))
		for _, s := range f.Sessions {
			src = append(src, s)
		}
	}
	out := make([]SessionInfo, 0, len(src))
	for _, s := range src {
		if s.Managed {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *FakeAPI) GetSession(_ context.Context, id string) (SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetCalls++
	if f.GetErr != nil {
		return SessionInfo{}, f.GetErr
	}
	s, ok := f.Sessions[id]
	if !ok {
		return SessionInfo{}, ErrSessionNotFound
	}
	return s, nil
}

// SetSession mutates fresh state under the lock (tests driving transitions).
func (f *FakeAPI) SetSession(s SessionInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Sessions == nil {
		f.Sessions = map[string]SessionInfo{}
	}
	f.Sessions[s.ID] = s
}

// DeleteSession removes a session so Get reports ErrSessionNotFound.
func (f *FakeAPI) DeleteSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Sessions, id)
}
