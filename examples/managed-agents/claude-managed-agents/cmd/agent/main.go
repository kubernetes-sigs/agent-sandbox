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

// Command agent creates a Claude Managed Agents agent and opens a session on
// a self-hosted environment, optionally seeding it with an initial prompt.
// The session's tool calls are executed by the controller and runner of this
// example, inside Agent Sandbox pods.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func main() {
	var (
		prompt    string
		model     string
		agentID   string
		sessionID string
	)
	defaultModel := os.Getenv("ANTHROPIC_MODEL")
	if defaultModel == "" {
		defaultModel = anthropic.BetaManagedAgentsModelClaudeOpus5
	}

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.StringVar(&agentID, "agent", "", "existing agent to reuse; a new agent is created when empty")
	flag.StringVar(&sessionID, "session", "", "existing session to send the prompt to; a new session is created when empty")
	flag.Parse()

	ctx := context.Background()
	// Credentials resolve in order: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, an
	// 'ant auth login' profile, then workload identity federation.
	client := anthropic.NewClient()

	// With --session the prompt continues an existing session: the
	// controller keys sandboxes by session ID, so this is the path that
	// resumes a suspended sandbox and its workspace.
	if sessionID != "" {
		if prompt == "" {
			fmt.Println("Error: --prompt is required with --session.")
			os.Exit(1)
		}
		sendToSession(ctx, &client, sessionID, prompt)
		fmt.Printf("Prompt:       %s\n", prompt)
		fmt.Printf("Session:      %s\n", sessionID)
		fmt.Printf("Session URL:  %s\n", sessionURL(sessionID))
		return
	}

	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	if envID == "" {
		fmt.Println("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.")
		fmt.Println("Export it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
		os.Exit(1)
	}

	session, resolvedAgentID := createSession(ctx, &client, envID, agentID, model, prompt)

	fmt.Printf("Prompt:       %s\n", prompt)
	fmt.Printf("Model:        %s\n", model)
	fmt.Printf("Agent:        %s\n", resolvedAgentID)
	fmt.Printf("Session:      %s\n", session.ID)
	fmt.Printf("Environment:  %s\n", envID)
	fmt.Printf("Session URL:  %s\n", sessionURL(session.ID))
}

func sessionURL(sessionID string) string {
	return fmt.Sprintf("https://platform.claude.com/workspaces/default/sessions/%s", sessionID)
}

// sendToSession posts a user message to an existing session, which produces
// a new work item for the controller to claim.
func sendToSession(ctx context.Context, client *anthropic.Client, sessionID, prompt string) {
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{
			anthropic.BetaManagedAgentsEventParamsOfUserMessage(
				[]anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
					{
						OfText: &anthropic.BetaManagedAgentsTextBlockParam{
							Type: "text",
							Text: prompt,
						},
					},
				},
			),
		},
	})
	if err != nil {
		log.Fatalf("failed to send prompt to session %s: %v", sessionID, err)
	}
}

// createSession creates the agent (unless one was supplied) and opens a
// session on the self-hosted environment.
func createSession(ctx context.Context, client *anthropic.Client, envID, agentID, model, prompt string) (*anthropic.BetaManagedAgentsSession, string) {
	if agentID == "" {
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name: "Agent Sandbox Example Agent",
			Model: anthropic.BetaManagedAgentsModelConfigParams{
				ID: anthropic.BetaManagedAgentsModel(model),
			},
			Tools: []anthropic.BetaAgentNewParamsToolUnion{
				{
					OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
						Type: "agent_toolset_20260401",
						DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
							// This demo approves every tool call unattended.
							PermissionPolicy: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParamsPermissionPolicyUnion{
								OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{
									Type: "always_allow",
								},
							},
						},
					},
				},
			},
		})
		if err != nil {
			log.Fatalf("failed to create agent: %v", err)
		}
		agentID = agent.ID
	}

	params := anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(agentID),
		},
		EnvironmentID: envID,
	}
	if prompt != "" {
		params.InitialEvents = []anthropic.BetaSessionNewParamsInitialEventUnion{
			{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Type: "user.message",
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
						{
							OfText: &anthropic.BetaManagedAgentsTextBlockParam{
								Type: "text",
								Text: prompt,
							},
						},
					},
				},
			},
		}
	}

	session, err := client.Beta.Sessions.New(ctx, params)
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	return session, agentID
}
