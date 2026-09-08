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
		err := sendPrompt(ctx, client, cons, sessionID, opt.Prompt)
		cons.flush() // don't lose streamed output when the process exits
		return err
	}

	// Interactive prompt loop.
	fmt.Println(`Type a prompt and press Enter ("exit" or Ctrl-D to quit).`)
	for {
		line, ok := cons.ask("\n> ")
		if !ok {
			fmt.Println()
			return nil
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

// withAuthRetry invokes fn, and if it fails with an auth-required error on
// an agent that advertises authentication methods, authenticates and
// retries once. Agents reject session/new and session/load with
// acp.AuthRequired until authenticate succeeds; any other error is
// returned as-is.
func withAuthRetry(ctx context.Context, client *acp.Client, initResp *acp.InitializeResponse, authMethod string, fn func() error) error {
	err := fn()
	if err == nil || len(initResp.AuthMethods) == 0 {
		return err
	}
	if !acp.IsAuthRequiredError(err) {
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

// console shows output and asks questions using a model / view split: the
// model is an ordered list of items appended by any goroutine, and a single
// view goroutine (run) — the only writer to stdout and reader of stdin —
// handles the items in order, keeping track of how far it has gotten.
//
// Producers never block on the user: while the view is waiting for the
// answer to a question, later items simply accumulate in the model. A
// question therefore holds back everything appended after it, so it is
// never pushed off the screen. (A TUI could replace the view — for example
// painting new items above the active question — without touching the
// model or its producers.)
type console struct {
	// workDir is the session working directory (symlinks resolved); agent
	// file system requests are confined to it.
	workDir string
	debug   bool
	// autoApprove selects the first "allow" option of every permission
	// request instead of asking the user.
	autoApprove bool

	// mu guards items; itemsChanged signals the view that items has grown.
	mu           sync.Mutex
	itemsChanged *sync.Cond
	// items is the model: output to print and questions to ask, in order.
	items []*item

	// toolTitles maps toolCallId → title, for labeling status updates. It
	// is only accessed from handleNotification, which acp.Client invokes on
	// its single read-loop goroutine.
	toolTitles map[string]string
}

// item is one entry in the console model: output to show and, optionally, a
// question whose answer should be read from the user.
type item struct {
	// text is printed verbatim.
	text string
	// needsLineStart means text must begin at the start of a line; a
	// newline is inserted first if streamed agent text left the cursor
	// mid-line.
	needsLineStart bool

	// question means one line of user input is read after printing text.
	question bool

	// handled, if non-nil, is closed by the view once the item has been
	// printed and any question answered; answer and eof are valid after
	// that. Producers that need the result (or need to sequence their next
	// step after the print) set it.
	handled chan struct{}
	answer  string
	eof     bool
}

// newConsole creates the console and starts its view goroutine.
func newConsole(workDir string, debug, autoApprove bool) *console {
	c := &console{
		workDir:     workDir,
		debug:       debug,
		autoApprove: autoApprove,
		toolTitles:  make(map[string]string),
	}
	c.itemsChanged = sync.NewCond(&c.mu)
	go c.run()
	return c
}

// append adds an item to the model and wakes the view.
func (c *console) append(it *item) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, it)
	c.itemsChanged.Broadcast()
}

// render appends output to the model.
func (c *console) render(text string, needsLineStart bool) {
	c.append(&item{text: text, needsLineStart: needsLineStart})
}

// ask appends a question to the model and blocks until the view has read
// the user's reply, returned without its trailing newline. ok is false if
// standard input has ended.
func (c *console) ask(prompt string) (line string, ok bool) {
	it := &item{text: prompt, needsLineStart: true, question: true, handled: make(chan struct{})}
	c.append(it)
	<-it.handled
	return it.answer, !it.eof
}

// endTurn renders the turn's stop reason. There is no need to wait for it
// to print: items are handled in append order, so it is shown before any
// question the caller asks next.
func (c *console) endTurn(stopReason string) {
	c.render(fmt.Sprintf("[turn ended: %s]\n", stopReason), true)
}

// flush blocks until the view has handled every item appended so far; call
// it before exiting so pending output is not lost.
func (c *console) flush() {
	it := &item{handled: make(chan struct{})}
	c.append(it)
	<-it.handled
}

// run is the view goroutine; see the console type comment.
func (c *console) run() {
	stdin := bufio.NewReader(os.Stdin)
	stdinClosed := false
	midLine := false // streamed text left the cursor mid-line
	handledCount := 0

	for {
		c.mu.Lock()
		for handledCount >= len(c.items) {
			c.itemsChanged.Wait()
		}
		it := c.items[handledCount]
		handledCount++
		// Once the view has caught up, drop the handled prefix so the
		// model does not grow without bound. clear releases the handled
		// item pointers still referenced by the backing array.
		if handledCount == len(c.items) {
			clear(c.items)
			c.items = c.items[:0]
			handledCount = 0
		}
		c.mu.Unlock()

		if it.needsLineStart && midLine {
			fmt.Println()
			midLine = false
		}
		if it.text != "" {
			fmt.Print(it.text)
			midLine = !strings.HasSuffix(it.text, "\n")
		}

		if it.question {
			if stdinClosed {
				it.eof = true
			} else {
				line, err := stdin.ReadString('\n')
				if err != nil {
					stdinClosed = true
				}
				line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
				if line == "" && err != nil {
					it.eof = true
				} else {
					it.answer = line
				}
			}
			// Interactively, the user's Enter moved to a fresh line; with
			// piped input the cursor is still after the prompt, and
			// continuing from there matches ordinary echoed input.
			midLine = false
		}

		if it.handled != nil {
			close(it.handled)
		}
	}
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

	update := notif.Update
	switch update.SessionUpdateKind {
	case acp.UpdateAgentMessageChunk:
		if text := textContent(update.Content); text != "" {
			c.render(text, false)
		}
	case acp.UpdateAgentThoughtChunk:
		if !c.debug {
			return
		}
		if text := textContent(update.Content); text != "" {
			c.render(fmt.Sprintf("[thought] %s\n", strings.TrimSpace(text)), true)
		}
	case acp.UpdateUserMessageChunk:
		// Replay of a prompt, e.g. while loading an existing session.
		if text := textContent(update.Content); text != "" {
			c.render(fmt.Sprintf("[user] %s\n", strings.TrimSpace(text)), true)
		}
	case acp.UpdateToolCall:
		c.toolTitles[update.ToolCallID] = update.Title
		c.render(fmt.Sprintf("[tool: %s] %s (%s)\n", update.ToolKind, update.Title, update.Status), true)
	case acp.UpdateToolCallUpdate:
		if update.Status == "" {
			return
		}
		title := c.toolTitles[update.ToolCallID]
		if title == "" {
			title = update.ToolCallID
		}
		c.render(fmt.Sprintf("[tool] %s → %s\n", title, update.Status), true)
	case acp.UpdatePlan:
		var plan strings.Builder
		plan.WriteString("[plan]\n")
		for _, entry := range update.Entries {
			fmt.Fprintf(&plan, "  - [%s] %s\n", entry.Status, entry.Content)
		}
		c.render(plan.String(), true)
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
// permission option to select for a tool call. The console goroutine asks
// questions one at a time and defers other output while one is pending, so
// concurrent requests cannot interleave and the question is never pushed
// off the screen.
func (c *console) requestPermission(req acp.RequestPermissionParams) acp.RequestPermissionResult {
	title := req.ToolCall.Title
	if title == "" {
		title = req.ToolCall.ToolCallID
	}

	var header strings.Builder
	fmt.Fprintf(&header, "\n[permission] Agent wants to run: %s\n", title)
	if c.debug && len(req.ToolCall.RawInput) > 0 {
		fmt.Fprintf(&header, "  input: %s\n", string(req.ToolCall.RawInput))
	}

	if len(req.Options) == 0 {
		header.WriteString("  (agent offered no permission options; cancelling)\n")
		c.render(header.String(), true)
		return acp.RequestPermissionResult{
			Outcome: acp.PermissionOutcome{Outcome: acp.PermissionCancelled},
		}
	}

	if c.autoApprove {
		for _, opt := range req.Options {
			if strings.HasPrefix(opt.Kind, "allow") {
				fmt.Fprintf(&header, "  auto-approving (-yolo): %s\n", opt.Name)
				c.render(header.String(), true)
				return selected(opt)
			}
		}
	}

	for i, opt := range req.Options {
		fmt.Fprintf(&header, "  %d) %s [%s]\n", i+1, opt.Name, opt.Kind)
	}

	prompt := header.String() + fmt.Sprintf("Choose 1-%d (Enter = 1): ", len(req.Options))
	for {
		line, ok := c.ask(prompt)
		if !ok {
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
		prompt = fmt.Sprintf("Invalid choice.\nChoose 1-%d (Enter = 1): ", len(req.Options))
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
