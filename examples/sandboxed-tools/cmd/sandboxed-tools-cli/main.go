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

// sandboxed-tools demonstrates launching an agent sandbox only for tool execution
// and stopping it immediately after the tool execution completes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/klog/v2"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/agent"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/sessions"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

func main() {
	ctx := context.Background()

	// Set up signal handling
	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	{
		klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
		klog.InitFlags(klogFlags)

		// Add some (but not all) klog flags
		klogFlags.VisitAll(func(f *flag.Flag) {
			switch f.Name {
			case "v":
				flag.Var(f.Value, f.Name, f.Usage)
			}
		})
	}

	// The session name identifies this chat (and its sandbox and snapshots);
	// it is a CLI concern, passed explicitly to Harness.BuildSession.
	sessionName := os.Getenv("SESSION_NAME")
	if sessionName == "" {
		sessionName = "default"
	}

	var opts agent.RunOptions
	opts.InitDefaults()
	flag.StringVar(&sessionName, "session", sessionName, "session name")
	flag.StringVar(&opts.Namespace, "namespace", opts.Namespace, "namespace")
	flag.StringVar(&opts.Image, "image", opts.Image, "image")
	flag.StringVar(&opts.HomeDir, "homedir", opts.HomeDir, "Home directory in the sandbox; this is currently the only directory that we persist with snapshot/restore.")
	flag.DurationVar(&opts.ToolTimeout, "tool-timeout", opts.ToolTimeout, "Maximum duration a single tool invocation may run before it is cancelled (Go duration syntax, e.g. \"30s\", \"2m\"). <= 0 disables the timeout.")
	flag.Parse()

	log := klog.FromContext(ctx)

	if err := run(signalCtx, opts, sessionName); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "\n")
			log.V(1).Info("context cancelled")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sandboxed-tools: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts agent.RunOptions, sessionName string) error {
	log := klog.FromContext(ctx)

	if opts.HomeDir == "" {
		return fmt.Errorf("homeDir must not be empty")
	}

	if sessionName == "" {
		return fmt.Errorf("sessionName is required")
	}

	if err := sessions.ValidateSessionName(sessionName); err != nil {
		return fmt.Errorf("invalid sessionName %q: %w", sessionName, err)
	}

	llmClient, err := llm.NewFromEnv(opts.ModelName)
	if err != nil {
		return fmt.Errorf("failed to initialize llm client: %w", err)
	}

	restConfig, err := agent.GetRESTConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes configuration: %w", err)
	}

	sandboxClient, err := agent.NewSandboxClient(restConfig)
	if err != nil {
		return fmt.Errorf("failed to initialize sandbox client: %w", err)
	}

	defer func() {
		if err := sandboxClient.DeleteAllSandboxes(context.WithoutCancel(ctx)); err != nil {
			log.Error(err, "failed to delete all sandboxes")
		}
	}()

	toolsRegistry := tools.NewRegistry()
	toolsRegistry.ToolTimeout = opts.ToolTimeout
	toolsRegistry.Add(&tools.RunCommand{})

	toolsRegistry.Add(&tools.ListFilesTool{})
	toolsRegistry.Add(&tools.ReadFileTool{})
	toolsRegistry.Add(&tools.WriteFileTool{})

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}
	sessionsDir := filepath.Join(homeDir, ".local", "sandboxed-tools")
	sessionStore := sessions.NewFileStore(sessionsDir)

	harness := agent.NewHarness(llmClient, sandboxClient, toolsRegistry, opts)

	session, err := harness.BuildSession(ctx, sessionStore, sessionName)
	if err != nil {
		return fmt.Errorf("building session: %w", err)
	}

	return runREPL(ctx, harness, session)
}

// runREPL runs the interactive read-eval-print loop on the terminal.
func runREPL(ctx context.Context, harness *agent.Harness, session *agent.Session) error {
	isNewSession := len(session.Messages()) == 0

	if err := harness.EnsureSystemPrompt(ctx, session); err != nil {
		return fmt.Errorf("adding system prompt: %w", err)
	}

	if isNewSession {
		fmt.Println("================================================================================")
		fmt.Println("Welcome to the Sandboxed Tools example!")
		fmt.Printf("Session Name: %s\n", session.Name)
		fmt.Println("Type your message (or '/exit' or '/quit' to quit):")
		fmt.Println("================================================================================")
	} else {
		fmt.Println("================================================================================")
		fmt.Printf("Resumed session %q with %d messages in history:\n", session.Name, len(session.Messages()))
		fmt.Println("================================================================================")
		for _, msg := range session.Messages() {
			if msg.Role == "user" && msg.Content != nil {
				fmt.Printf("User> %s\n", *msg.Content)
			} else if msg.Role == "assistant" && msg.Content != nil && *msg.Content != "" {
				fmt.Printf("Agent> %s\n", *msg.Content)
			}
		}
	}

	for {
		fmt.Print("\nUser> ")
		lineBytes, err := readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("error reading standard input: %w", err)
		}

		input := strings.TrimSpace(string(lineBytes))
		if input == "" {
			continue
		}
		if strings.ToLower(input) == "/exit" || strings.ToLower(input) == "/quit" {
			return nil
		}

		if err := harness.RunTurn(ctx, session, input, &consoleEvents{}); err != nil {
			return err
		}
	}
}

// consoleEvents renders turn progress to the terminal. Tool calls are
// approved unconditionally, preserving the CLI's historical behavior.
type consoleEvents struct{}

func (e *consoleEvents) AssistantMessage(_ context.Context, text string) {
	fmt.Printf("\nAgent> %s\n", text)
}

func (e *consoleEvents) ApproveToolCall(_ context.Context, _ llm.ToolCall) (bool, error) {
	return true, nil
}

func (e *consoleEvents) ToolCallStarted(_ context.Context, _ llm.ToolCall) {}

func (e *consoleEvents) ToolCallFinished(_ context.Context, _ llm.ToolCall, _ error) {}

// readLine reads a single line from os.Stdin.
func readLine() ([]byte, error) {
	var line []byte

	buf := make([]byte, 1)
	for {
		_, err := os.Stdin.Read(buf)
		if err != nil {
			// io.EOF is the normal end of input (e.g. Ctrl-D); don't log it.
			if !errors.Is(err, io.EOF) {
				klog.Infof("failed to read line: %v", err)
			}
			return nil, fmt.Errorf("failed to read line: %w", err)
		}
		if buf[0] == '\n' {
			return line, nil
		}
		if buf[0] != '\r' {
			line = append(line, buf[0])
		}
	}
}
