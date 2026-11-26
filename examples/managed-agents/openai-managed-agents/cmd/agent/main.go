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

// Command agent starts an OpenAI Agents API session on a self-hosted
// environment and sends it a prompt — the trigger that makes the controller
// claim a sandbox and connect the session's executor. Sending a follow-up
// prompt to an existing session (--session) exercises the resume path: the
// sandbox was suspended after its idle threshold, and the new turn brings it
// back with the workspace volume reattached.
//
//	export OPENAI_API_KEY=sk-...
//	go run ./cmd/agent --prompt "Run 'uname -a' in bash and show the output."
//	go run ./cmd/agent --session <id> --prompt "What files are in the workspace?"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/openai-managed-agents/internal/controller"
)

func main() {
	var (
		prompt  = flag.String("prompt", "Run 'uname -a' in bash and show the output.", "prompt to send")
		session = flag.String("session", "", "existing session ID to send a follow-up prompt to (default: create a new session)")
		workdir = flag.String("workdir", "/workspace", "working directory inside the self-hosted environment")
		model   = flag.String("model", "gpt-5.1-codex-max", "agent model (only used when creating a session)")
	)
	flag.Parse()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY is required.")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	client := openai.NewClient(option.WithAPIKey(apiKey))

	sessionID := *session
	if sessionID == "" {
		sess, err := client.Beta.Agents.Sessions.New(ctx, openai.BetaAgentSessionNewParams{
			Environment: openai.EnvironmentParamOfParamSelfHosted(*workdir),
			Metadata: map[string]string{
				controller.MetadataManagedByKey: controller.MetadataManagedByValue,
			},
			Agent: openai.BetaAgentSessionNewParamsAgent{
				Model: openai.String(*model),
			},
			Input: openai.BetaAgentSessionNewParamsInputUnion{
				OfString: openai.String(*prompt),
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Creating session failed: %v\n", err)
			os.Exit(1)
		}
		sessionID = sess.ID
		fmt.Printf("Session:      %s\n", sess.ID)
		fmt.Printf("Environment:  %s (self_hosted, workdir %s)\n", sess.Environment.ID, *workdir)
		fmt.Printf("Platform URL: https://platform.openai.com/agents/sessions/%s\n", sess.ID)
		fmt.Println("The controller will claim a sandbox and connect the session's executor;")
		fmt.Println("watch it with: kubectl get sandbox -w")
	} else {
		stream := client.Beta.Agents.Sessions.Stream(ctx, sessionID, openai.AgentSessionStreamParams{
			Input: *prompt,
		})
		defer stream.Close()
		fmt.Printf("Follow-up sent to session %s.\n", sessionID)
		fmt.Println("If its sandbox was suspended, the controller resumes it and reconnects the executor.")
		// Print turn events until the stream ends so the follow-up is visible
		// without opening the platform UI.
		for stream.Next() {
			ev := stream.Current()
			fmt.Printf("event: %s\n", ev.Type)
		}
		if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "Streaming session events failed: %v\n", err)
			os.Exit(1)
		}
	}
}
