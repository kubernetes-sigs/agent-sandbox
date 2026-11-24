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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/bma"
)

func awsTestAPI(t *testing.T, handler http.Handler) AgentAPI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", ""),
	}
	c, err := bma.NewClient(context.Background(), bma.Options{
		Region: "us-east-1", Endpoint: srv.URL, AccountID: "123456789012", AWSConfig: &cfg,
	})
	if err != nil {
		t.Fatalf("bma.NewClient: %v", err)
	}
	return NewAWSAPI(c)
}

func sessionJSON(id, status, instructions, envType, envID string) map[string]any {
	return map[string]any{
		"id":     id,
		"object": "agent.session",
		"agent":  map[string]any{"model": "openai.gpt-5.6-luna", "instructions": instructions},
		"environment": map[string]any{
			"id": envID, "type": envType, "workspace_directory": "/workspace",
		},
		"status": status,
	}
}

func writeJSONBody(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func TestListManagedSessionsFiltersMarker(t *testing.T) {
	api := awsTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/agents/sessions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSONBody(t, w, map[string]any{
			"data": []any{
				sessionJSON("sess_managed", "idle", "Do things.\n\n"+InstructionsMarker, "self_hosted", "env_1"),
				sessionJSON("sess_other", "idle", "Do things.", "self_hosted", "env_2"),
			},
			"has_more": false,
		})
	}))
	got, err := api.ListManagedSessions(context.Background())
	if err != nil {
		t.Fatalf("ListManagedSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sess_managed" {
		t.Fatalf("managed sessions = %+v", got)
	}
	if !got[0].Managed || got[0].EnvironmentID != "env_1" || got[0].Status != "idle" || got[0].ActiveTurn {
		t.Errorf("mapping = %+v", got[0])
	}
}

func TestListManagedSessionsPaginates(t *testing.T) {
	// Page 1 returns has_more + last_id; the client must follow with after=.
	var pages int
	api := awsTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("after") == "" {
			writeJSONBody(t, w, map[string]any{
				"data":     []any{sessionJSON("sess_a", "idle", InstructionsMarker, "self_hosted", "env_a")},
				"has_more": true, "last_id": "cur_1",
			})
			return
		}
		if r.URL.Query().Get("after") != "cur_1" {
			t.Errorf("after = %q", r.URL.Query().Get("after"))
		}
		writeJSONBody(t, w, map[string]any{
			"data":     []any{sessionJSON("sess_b", "in_progress", InstructionsMarker, "self_hosted", "env_b")},
			"has_more": false,
		})
	}))
	got, err := api.ListManagedSessions(context.Background())
	if err != nil {
		t.Fatalf("ListManagedSessions: %v", err)
	}
	if pages != 2 || len(got) != 2 {
		t.Fatalf("pages = %d, sessions = %d", pages, len(got))
	}
	if got[1].ID != "sess_b" || !got[1].ActiveTurn || got[1].Status != "in_progress" {
		t.Errorf("second session = %+v", got[1])
	}
}

func TestGetSessionMapsFields(t *testing.T) {
	api := awsTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/agents/sessions/sess_1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSONBody(t, w, sessionJSON("sess_1", "in_progress", "x "+InstructionsMarker, "self_hosted", "env_1"))
	}))
	info, err := api.GetSession(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !info.Managed || !info.ActiveTurn || info.EnvironmentID != "env_1" {
		t.Errorf("info = %+v", info)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	api := awsTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"no such session"}}`))
	}))
	_, err := api.GetSession(context.Background(), "gone")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestGetSessionIgnoresNonSelfHostedEnv(t *testing.T) {
	api := awsTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, sessionJSON("sess_1", "idle", InstructionsMarker, "aws_bedrock_agentcore", "env_1"))
	}))
	info, err := api.GetSession(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if info.EnvironmentID != "" {
		t.Errorf("EnvironmentID = %q for a non-self-hosted environment", info.EnvironmentID)
	}
}
