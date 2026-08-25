/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command acp-client is a terminal front end for an Agent Client Protocol
// (ACP) agent such as `gemini --acp`.
//
// It spawns the agent as a subprocess, creates or resumes a session, and
// runs an interactive prompt loop. While a prompt is being processed, the
// agent's streamed output is rendered to the terminal and tool call
// permission requests are answered by the user.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/agent-sandbox/examples/agentclientprotocol/pkg/acp"
)

type options struct {
	// AgentCommand is the command line used to spawn the ACP agent subprocess.
	AgentCommand string
	// WorkingDirectory is the session working directory; file system requests
	// from the agent are confined to it. Empty means the current directory.
	WorkingDirectory string
	// SessionID, if set, resumes an existing session instead of creating one.
	SessionID string
	// Prompt, if set, is sent as a single prompt turn instead of running the
	// interactive loop.
	Prompt string
	// AuthMethod is the authentication method ID to use if the agent requires
	// auth; empty selects the first method the agent advertises.
	AuthMethod string
	// AutoApprove approves every tool call permission request without asking.
	AutoApprove bool
	// Debug shows agent stderr, thoughts, and raw notification traffic.
	Debug bool
	// SetupTimeout bounds the initialize/authenticate/session setup calls.
	// Prompt turns are not subject to a timeout.
	SetupTimeout time.Duration
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	var opt options
	flag.StringVar(&opt.AgentCommand, "cmd", "gemini --acp", "Command to start the ACP agent (e.g. 'gemini --acp')")
	flag.StringVar(&opt.WorkingDirectory, "cwd", "", "Working directory for the session (defaults to current directory)")
	flag.StringVar(&opt.SessionID, "session-id", "", "Resume an existing session ID instead of creating a new one")
	flag.StringVar(&opt.Prompt, "prompt", "", "Send a single prompt and exit instead of running interactively")
	flag.StringVar(&opt.AuthMethod, "auth-method", "", "Authentication method ID to use if the agent requires auth (defaults to the first advertised method)")
	flag.BoolVar(&opt.AutoApprove, "yolo", false, "Automatically approve all tool call permission requests")
	flag.BoolVar(&opt.Debug, "debug", false, "Show agent stderr, thoughts, and raw notification traffic")
	flag.DurationVar(&opt.SetupTimeout, "setup-timeout", 60*time.Second, "Timeout for initialize/authenticate/session setup calls")
	flag.Parse()

	cwd := opt.WorkingDirectory
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current working directory: %w", err)
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}
	// Resolve symlinks (e.g. /tmp → /private/tmp on macOS) so that the
	// paths the agent sends back compare correctly against cwd.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	agentOut, agentIn, cleanup, err := connectAgent(ctx, opt, cwd)
	if err != nil {
		return err
	}
	defer cleanup()

	cons := newConsole(cwd, opt.Debug, opt.AutoApprove)

	client := acp.NewClient(agentOut, agentIn)
	client.OnNotification = cons.handleNotification
	client.OnRequest = cons.handleRequest
	go client.Run()

	setupCtx, cancel := context.WithTimeout(ctx, opt.SetupTimeout)
	defer cancel()

	sessionID, err := setupSession(setupCtx, client, opt, cwd)
	if err != nil {
		return err
	}

	// One-shot mode: send a single prompt and exit.
	if opt.Prompt != "" {
		return sendPrompt(ctx, client, cons, sessionID, opt.Prompt)
	}

	// Interactive prompt loop.
	fmt.Println(`Type a prompt and press Enter ("exit" or Ctrl-D to quit).`)
	for {
		fmt.Print("\n> ")
		line, err := cons.stdin.ReadString('\n')
		if err == io.EOF {
			fmt.Println()
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if err := sendPrompt(ctx, client, cons, sessionID, line); err != nil {
			fmt.Fprintf(os.Stderr, "prompt failed: %v\n", err)
		}
	}
}

// connectAgent spawns the configured agent command and returns the reader
// and writer connected to its stdout and stdin.
func connectAgent(ctx context.Context, opt options, cwd string) (io.Reader, io.Writer, func(), error) {
	args := strings.Fields(opt.AgentCommand)
	if len(args) == 0 {
		return nil, nil, nil, fmt.Errorf("empty agent command")
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	if opt.Debug {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting agent process (%s): %w", opt.AgentCommand, err)
	}

	cleanup := func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return stdout, stdin, cleanup, nil
}

// setupSession initializes the ACP connection and creates or resumes a
// session, authenticating first if the agent requires it.
func setupSession(ctx context.Context, client *acp.Client, opt options, cwd string) (string, error) {
	initResp, err := client.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion,
		ClientCapabilities: acp.ClientCapabilities{
			FS: acp.FSCapabilities{ReadTextFile: true, WriteTextFile: true},
		},
		ClientInfo: &acp.ClientInfo{Name: "simple-acp-client", Version: "0.1.0"},
	})
	if err != nil {
		return "", fmt.Errorf("ACP initialize failed: %w", err)
	}

	fmt.Printf("Connected to ACP agent (protocol v%d)\n", initResp.ProtocolVersion)
	if initResp.AgentInfo != nil {
		fmt.Printf("  Agent: %s %s\n", initResp.AgentInfo.Name, initResp.AgentInfo.Version)
	}

	if opt.SessionID != "" {
		fmt.Printf("Loading session %s...\n", opt.SessionID)
		err := withAuthRetry(ctx, client, initResp, opt.AuthMethod, func() error {
			return client.LoadSession(ctx, acp.LoadSessionRequest{SessionID: opt.SessionID, CWD: cwd})
		})
		if err != nil {
			return "", fmt.Errorf("loading session %s: %w", opt.SessionID, err)
		}
		return opt.SessionID, nil
	}

	var newResp *acp.NewSessionResponse
	err = withAuthRetry(ctx, client, initResp, opt.AuthMethod, func() error {
		var err error
		newResp, err = client.NewSession(ctx, acp.NewSessionRequest{CWD: cwd})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}
	fmt.Printf("Created session %s\n", newResp.SessionID)
	return newResp.SessionID, nil
}

// withAuthRetry invokes fn, and if it fails on an agent that advertises
// authentication methods, authenticates and retries once. Agents reject
// session/new and session/load until authenticate succeeds.
func withAuthRetry(ctx context.Context, client *acp.Client, initResp *acp.InitializeResponse, authMethod string, fn func() error) error {
	err := fn()
	if err == nil || len(initResp.AuthMethods) == 0 {
		return err
	}
	method := authMethod
	if method == "" {
		method = initResp.AuthMethods[0].ID
	}
	fmt.Printf("Request failed (%v); authenticating with method %q...\n", err, method)
	if err := client.Authenticate(ctx, acp.AuthenticateRequest{MethodID: method}); err != nil {
		return fmt.Errorf("authenticate (%s) failed: %w", method, err)
	}
	return fn()
}

// sendPrompt runs one prompt turn, leaving the console at the start of a
// fresh line afterwards.
func sendPrompt(ctx context.Context, client *acp.Client, cons *console, sessionID, text string) error {
	result, err := client.Prompt(ctx, acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: text}},
	})
	if err != nil {
		return err
	}
	cons.endTurn(result.StopReason)
	return nil
}

// console renders session updates and answers agent requests (permission
// prompts, file system access). It owns stdin/stdout for the interactive
// loop.
type console struct {
	stdin *bufio.Reader
	// workDir is the session working directory (symlinks resolved); agent
	// file system requests are confined to it.
	workDir string
	debug   bool
	// autoApprove selects the first "allow" option of every permission
	// request instead of asking the user.
	autoApprove bool

	mu             sync.Mutex
	midAgentOutput bool              // true while streamed agent text lacks a trailing newline
	toolTitles     map[string]string // toolCallId → title, for labeling status updates
}

func newConsole(workDir string, debug, autoApprove bool) *console {
	return &console{
		stdin:       bufio.NewReader(os.Stdin),
		workDir:     workDir,
		debug:       debug,
		autoApprove: autoApprove,
		toolTitles:  make(map[string]string),
	}
}

// breakAgentOutput ensures we are at the start of a line before printing
// non-chunk output (tool status, prompts). Callers must hold c.mu.
func (c *console) breakAgentOutput() {
	if c.midAgentOutput {
		fmt.Println()
		c.midAgentOutput = false
	}
}

// endTurn prints the turn's stop reason once the prompt call returns.
func (c *console) endTurn(stopReason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breakAgentOutput()
	fmt.Printf("[turn ended: %s]\n", stopReason)
}

// textContent decodes a session update's content as a single text block,
// returning "" if it is anything else.
func textContent(content json.RawMessage) string {
	var block acp.ContentBlock
	if err := json.Unmarshal(content, &block); err != nil {
		return ""
	}
	return block.Text
}

func (c *console) handleNotification(method string, params json.RawMessage) {
	if method != acp.MethodSessionUpdate {
		if c.debug {
			fmt.Fprintf(os.Stderr, "[debug] notification %s: %s\n", method, string(params))
		}
		return
	}

	var notif acp.SessionUpdateNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing session/update: %v\n", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	update := notif.Update
	switch update.SessionUpdateKind {
	case acp.UpdateAgentMessageChunk:
		if text := textContent(update.Content); text != "" {
			fmt.Print(text)
			c.midAgentOutput = !strings.HasSuffix(text, "\n")
		}
	case acp.UpdateAgentThoughtChunk:
		if !c.debug {
			return
		}
		if text := textContent(update.Content); text != "" {
			c.breakAgentOutput()
			fmt.Printf("[thought] %s\n", strings.TrimSpace(text))
		}
	case acp.UpdateUserMessageChunk:
		// Replay of a prompt, e.g. while loading an existing session.
		if text := textContent(update.Content); text != "" {
			c.breakAgentOutput()
			fmt.Printf("[user] %s\n", strings.TrimSpace(text))
		}
	case acp.UpdateToolCall:
		c.breakAgentOutput()
		c.toolTitles[update.ToolCallID] = update.Title
		fmt.Printf("[tool: %s] %s (%s)\n", update.ToolKind, update.Title, update.Status)
	case acp.UpdateToolCallUpdate:
		if update.Status == "" {
			return
		}
		c.breakAgentOutput()
		title := c.toolTitles[update.ToolCallID]
		if title == "" {
			title = update.ToolCallID
		}
		fmt.Printf("[tool] %s → %s\n", title, update.Status)
	case acp.UpdatePlan:
		c.breakAgentOutput()
		fmt.Println("[plan]")
		for _, entry := range update.Entries {
			fmt.Printf("  - [%s] %s\n", entry.Status, entry.Content)
		}
	default:
		if c.debug {
			fmt.Fprintf(os.Stderr, "[debug] session update %s\n", update.SessionUpdateKind)
		}
	}
}

// handleRequest answers agent → client requests.
func (c *console) handleRequest(method string, params json.RawMessage) (any, *acp.RPCError) {
	switch method {
	case acp.MethodRequestPermission:
		var req acp.RequestPermissionParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return c.requestPermission(req), nil

	case acp.MethodReadTextFile:
		var req acp.ReadTextFileParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		if req.Line != nil && *req.Line < 1 {
			return nil, invalidParams(fmt.Errorf("line must be >= 1, got %d", *req.Line))
		}
		if req.Limit != nil && *req.Limit < 0 {
			return nil, invalidParams(fmt.Errorf("limit must be >= 0, got %d", *req.Limit))
		}
		path, err := c.resolvePath(req.Path)
		if err != nil {
			return nil, internalError(err)
		}
		req.Path = path
		content, err := readTextFile(req)
		if err != nil {
			return nil, internalError(err)
		}
		return acp.ReadTextFileResult{Content: content}, nil

	case acp.MethodWriteTextFile:
		var req acp.WriteTextFileParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		path, err := c.resolvePath(req.Path)
		if err != nil {
			return nil, internalError(err)
		}
		if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
			return nil, internalError(err)
		}
		return struct{}{}, nil

	default:
		return nil, &acp.RPCError{Code: acp.MethodNotFound, Message: fmt.Sprintf("method not supported: %s", method)}
	}
}

func invalidParams(err error) *acp.RPCError {
	return &acp.RPCError{Code: acp.InvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
}

func internalError(err error) *acp.RPCError {
	return &acp.RPCError{Code: acp.InternalError, Message: err.Error()}
}

// resolvePath makes an agent-supplied path absolute (relative paths are
// resolved against the session working directory) and rejects paths that
// escape the working directory, so a misbehaving agent cannot read or write
// arbitrary files on the client.
func (c *console) resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.workDir, path)
	}
	path = filepath.Clean(path)
	// Resolve symlinks on the file itself, so that a symlink inside the
	// working directory cannot point a read or write outside of it (and so
	// that equivalent paths, e.g. /tmp vs /private/tmp on macOS, compare
	// correctly below). The file may not exist yet for a write; in that
	// case resolve its parent directory instead.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else {
		dir, base := filepath.Split(path)
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", fmt.Errorf("resolving %q: %w", path, err)
		}
		path = filepath.Join(resolvedDir, base)
	}
	rel, err := filepath.Rel(c.workDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the session working directory %q", path, c.workDir)
	}
	return path, nil
}

// requestPermission asks the user (or auto-approves with -yolo) which
// permission option to select for a tool call. Concurrent requests are
// serialized by c.mu, so at most one prompt reads stdin at a time.
func (c *console) requestPermission(req acp.RequestPermissionParams) acp.RequestPermissionResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breakAgentOutput()

	title := req.ToolCall.Title
	if title == "" {
		title = req.ToolCall.ToolCallID
	}
	fmt.Printf("\n[permission] Agent wants to run: %s\n", title)
	if c.debug && len(req.ToolCall.RawInput) > 0 {
		fmt.Printf("  input: %s\n", string(req.ToolCall.RawInput))
	}

	if len(req.Options) == 0 {
		fmt.Println("  (agent offered no permission options; cancelling)")
		return acp.RequestPermissionResult{
			Outcome: acp.PermissionOutcome{Outcome: acp.PermissionCancelled},
		}
	}

	if c.autoApprove {
		for _, opt := range req.Options {
			if strings.HasPrefix(opt.Kind, "allow") {
				fmt.Printf("  auto-approving (-yolo): %s\n", opt.Name)
				return selected(opt)
			}
		}
	}

	for i, opt := range req.Options {
		fmt.Printf("  %d) %s [%s]\n", i+1, opt.Name, opt.Kind)
	}
	for {
		fmt.Printf("Choose 1-%d (Enter = 1): ", len(req.Options))
		line, err := c.stdin.ReadString('\n')
		if err != nil {
			return acp.RequestPermissionResult{
				Outcome: acp.PermissionOutcome{Outcome: acp.PermissionCancelled},
			}
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return selected(req.Options[0])
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(req.Options) {
			return selected(req.Options[n-1])
		}
		fmt.Println("Invalid choice.")
	}
}

func selected(opt acp.PermissionOption) acp.RequestPermissionResult {
	return acp.RequestPermissionResult{
		Outcome: acp.PermissionOutcome{Outcome: acp.PermissionSelected, OptionID: opt.OptionID},
	}
}

// readTextFile serves an fs/read_text_file request, optionally returning
// only the requested line range.
func readTextFile(req acp.ReadTextFileParams) (string, error) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return "", err
	}
	content := string(data)
	if req.Line == nil && req.Limit == nil {
		return content, nil
	}

	lines := strings.Split(content, "\n")
	start := 0
	if req.Line != nil && *req.Line > 1 {
		start = min(*req.Line-1, len(lines))
	}
	end := len(lines)
	if req.Limit != nil {
		end = min(start+*req.Limit, len(lines))
	}
	return strings.Join(lines[start:end], "\n"), nil
}
