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

// Command agent starts a BMA session on a self-hosted environment and sends
// it a prompt — the trigger that makes the controller claim a sandbox and
// attach the session's executor. Sending a follow-up prompt to an existing
// session (--session) exercises the resume path: the sandbox was suspended
// after its idle threshold, and the new turn brings it back with the
// workspace volume reattached.
//
//	export AWS_PROFILE=my-bma-client-profile
//	export BMA_INFERENCE_ROLE_ARN=arn:aws:iam::123456789012:role/BedrockManagedAgentsPreviewInferenceServiceRole
//	go run ./cmd/agent --prompt "Run 'uname -a' in bash and show the output."
//	go run ./cmd/agent --session <id> --prompt "What files are in the workspace?"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/bma"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/controller"
)

func main() {
	var (
		prompt  = flag.String("prompt", "Run 'uname -a' in bash and show the output.", "prompt to send")
		session = flag.String("session", "", "existing session ID to send a follow-up prompt to (default: create a new session)")
		workdir = flag.String("workdir", "/workspace", "workspace directory inside the sandbox (must match the SandboxTemplate mount)")
		model   = flag.String("model", envOr("BMA_MODEL", "openai.gpt-5.6-luna"), "agent model (only used when creating a session)")
		region  = flag.String("region", envOr("BMA_REGION", envOr("AWS_REGION", "us-east-1")), "BMA region")
		roleArn = flag.String("role-arn", os.Getenv("BMA_INFERENCE_ROLE_ARN"), "IAM role ARN BMA assumes for the session (required when creating)")
		wait    = flag.Duration("wait", 5*time.Minute, "how long to wait for the turn to settle before printing items")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := bma.NewClient(ctx, bma.Options{Region: *region})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Building the BMA client failed: %v\n", err)
		os.Exit(1)
	}

	sessionID := *session
	if sessionID == "" {
		if *roleArn == "" {
			fmt.Fprintln(os.Stderr, "Error: --role-arn (or BMA_INFERENCE_ROLE_ARN) is required to create a session.")
			os.Exit(1)
		}
		sessionID = createSession(ctx, client, *model, *roleArn, *workdir, *prompt)
	} else {
		submitMessage(ctx, client, sessionID, *prompt)
		fmt.Printf("Follow-up sent to session %s.\n", sessionID)
		fmt.Println("If its sandbox was suspended, the controller resumes it and reattaches the executor.")
	}
	settled, err := waitForIdle(ctx, client, sessionID, *wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Waiting for the turn failed: %v\n", err)
		os.Exit(1)
	}
	if err := printLatestItems(ctx, client, sessionID, settled); err != nil {
		fmt.Fprintf(os.Stderr, "Reading the turn's items failed: %v\n", err)
		os.Exit(1)
	}
}

func createSession(ctx context.Context, c *bma.Client, model, roleArn, workdir, prompt string) string {
	// The marker rides in the instructions: BMA rejects unknown
	// create-session fields, so metadata tagging is not an option.
	body, err := json.Marshal(map[string]any{
		"agent": map[string]any{
			"model":        model,
			"instructions": "Use shell tools when explicitly requested. Report tool and command output exactly.\n\n" + controller.InstructionsMarker,
		},
		"environment": map[string]any{
			"type":                   "self_hosted",
			"workspace_directory":    workdir,
			"capability_directories": []string{strings.TrimSuffix(workdir, "/") + "/skills"},
		},
		"role_arn": roleArn,
		"input":    prompt,
		"stream":   false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Encoding the create-session request failed: %v\n", err)
		os.Exit(1)
	}
	var sess struct {
		ID          string `json:"id"`
		Environment struct {
			ID string `json:"id"`
		} `json:"environment"`
	}
	if err := c.Do(ctx, "POST", "/agents/sessions", nil, strings.NewReader(string(body)), &sess); err != nil {
		fmt.Fprintf(os.Stderr, "Creating session failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Session:     %s\n", sess.ID)
	fmt.Printf("Environment: %s (self_hosted, workdir %s)\n", sess.Environment.ID, workdir)
	fmt.Println("The controller will claim a sandbox and attach the session's executor;")
	fmt.Println("watch it with: kubectl get sandbox -w")
	return sess.ID
}

func submitMessage(ctx context.Context, c *bma.Client, sessionID, prompt string) {
	body, err := json.Marshal(map[string]any{
		"events": []any{map[string]any{
			"type": "agent.session.input.message",
			"input": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": prompt}},
			}},
		}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Encoding the message failed: %v\n", err)
		os.Exit(1)
	}
	if err := c.Do(ctx, "POST", "/agents/sessions/"+sessionID+"/events", nil, strings.NewReader(string(body)), nil); err != nil {
		fmt.Fprintf(os.Stderr, "Submitting the message failed: %v\n", err)
		os.Exit(1)
	}
}

// waitForIdle polls the session until its turn settles or the timeout
// expires, reporting which happened. Expiry is not an error: the caller
// still prints existing items.
func waitForIdle(ctx context.Context, c *bma.Client, sessionID string, timeout time.Duration) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for wctx.Err() == nil {
		var s struct {
			Status string `json:"status"`
		}
		if err := c.Do(wctx, "GET", "/agents/sessions/"+sessionID, nil, nil, &s); err != nil {
			if wctx.Err() != nil {
				break // the --wait deadline hit mid-request
			}
			return false, err
		}
		if s.Status != "in_progress" {
			fmt.Printf("Session status: %s\n", s.Status)
			return true, nil
		}
		select {
		case <-wctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err() // interrupted, not a --wait timeout
	}
	fmt.Fprintln(os.Stderr, "Timed out waiting for the turn to settle; printing items anyway.")
	return false, nil
}

type item struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	TurnID  string          `json:"turn_id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// Command-execution items:
	Command  []string `json:"command"`
	Output   string   `json:"output"`
	ExitCode *int     `json:"exit_code"`
}

// printLatestItems prints the newest turn's durable items, newest-first,
// paging until that turn's items are complete. BMA's submission ack carries
// no turn ID, so when settled is false the newest items may belong to an
// earlier turn; the output says so.
func printLatestItems(ctx context.Context, c *bma.Client, sessionID string, settled bool) error {
	var items []item
	latest := ""
	q := url.Values{"limit": {"100"}, "order": {"desc"}}
	// Bounded paging: a single turn cannot realistically span 1000 items.
	for page := 0; page < 10; page++ {
		var resp struct {
			Data    []item `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := c.Do(ctx, "GET", "/agents/sessions/"+sessionID+"/items", q, nil, &resp); err != nil {
			return err
		}
		done := false
		for _, it := range resp.Data {
			if len(items) == 0 {
				latest = it.TurnID
			}
			if latest != "" && it.TurnID != latest {
				done = true
				break
			}
			items = append(items, it)
		}
		if done || !resp.HasMore || resp.LastID == "" {
			break
		}
		q.Set("after", resp.LastID)
	}
	if len(items) == 0 {
		fmt.Println("No items yet.")
		return nil
	}
	if settled {
		fmt.Printf("Items from turn %s (newest first):\n", latest)
	} else {
		fmt.Printf("The submitted turn had not settled; the newest items (turn %s) may belong to an earlier turn:\n", latest)
	}
	for _, it := range items {
		switch {
		case it.ExitCode != nil:
			fmt.Printf("- %s: %v (exit %d)\n%s\n", it.Type, it.Command, *it.ExitCode, it.Output)
		case len(it.Content) > 0:
			fmt.Printf("- %s (%s): %s\n", it.Type, it.Role, it.Content)
		default:
			fmt.Printf("- %s\n", it.Type)
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
