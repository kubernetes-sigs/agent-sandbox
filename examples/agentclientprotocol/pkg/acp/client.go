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

// Package acp implements a minimal client for the Agent Client Protocol
// (https://agentclientprotocol.com).
//
// ACP is a JSON-RPC 2.0 protocol, spoken over newline-delimited JSON on a
// byte stream (typically the stdin/stdout of an agent process). The protocol
// is bidirectional: while the client's session/prompt request is pending, the
// agent streams session/update notifications and may issue its own requests
// back to the client (permission prompts, file system access). Client
// therefore runs a single reader goroutine that correlates responses to
// pending calls by request ID and dispatches agent-initiated traffic to
// caller-provided handlers.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// RequestHandler responds to a request sent by the agent to the client,
// such as session/request_permission or fs/read_text_file. It returns
// either a result to marshal into the response, or an *RPCError.
//
// Handlers are invoked on their own goroutine and may block (for example,
// waiting for the user to answer a permission prompt).
type RequestHandler func(method string, params json.RawMessage) (any, *RPCError)

// NotificationHandler receives notifications sent by the agent, such as
// session/update. It is invoked synchronously from the read loop, so it
// must not call back into the Client.
type NotificationHandler func(method string, params json.RawMessage)

// Client is an Agent Client Protocol client. Create one with NewClient,
// assign handlers, then start Run in a goroutine before issuing calls.
type Client struct {
	reader io.Reader

	writeMu sync.Mutex // serializes writes to writer
	writer  io.Writer

	pendingMu sync.Mutex
	nextID    int64
	// pending holds a completion callback per in-flight request ID, invoked
	// with the agent's response by the read loop. Keys are the string form
	// of the ID (see idKey), since JSON-RPC 2.0 permits both number and
	// string IDs and some agents echo numbers back as strings.
	pending map[string]func(message)

	// OnNotification, if set, receives agent notifications (session/update);
	// without it, notifications are ignored.
	// OnRequest, if set, answers agent-initiated requests; without it, all
	// such requests fail with MethodNotFound. Both must be assigned before
	// Run is started.
	OnNotification NotificationHandler
	OnRequest      RequestHandler

	done    chan struct{} // closed when the read loop exits
	readErr error         // reason the read loop exited; valid after done is closed
}

// NewClient returns a Client that reads agent output from r and writes
// requests to w.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{
		reader:  r,
		writer:  w,
		pending: make(map[string]func(message)),
		done:    make(chan struct{}),
	}
}

// Run reads and dispatches messages from the agent until the stream closes,
// then fails any pending calls. It must be running (normally on its own
// goroutine) for calls to complete.
func (c *Client) Run() {
	scanner := bufio.NewScanner(c.reader)
	const maxLineSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		// Tolerate agents that mix plain log lines into stdout.
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		c.dispatch(msg)
	}

	c.readErr = scanner.Err()
	if c.readErr == nil {
		c.readErr = io.EOF
	}
	close(c.done)
}

func (c *Client) dispatch(msg message) {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		// Agent → client request. Handled on its own goroutine because
		// handlers may block on user input while the agent continues to
		// stream notifications.
		go c.handleRequest(msg)
	case msg.Method != "":
		if c.OnNotification != nil {
			c.OnNotification(msg.Method, msg.Params)
		}
	case len(msg.ID) > 0:
		// Response to one of our requests.
		key := idKey(msg.ID)
		c.pendingMu.Lock()
		complete, ok := c.pending[key]
		delete(c.pending, key)
		c.pendingMu.Unlock()
		if ok {
			complete(msg)
		}
	}
}

// idKey normalizes a JSON-RPC ID to a map key: the value of a string ID, or
// the literal text of a number ID, so that 42 and "42" match.
func idKey(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func (c *Client) handleRequest(msg message) {
	var result any
	rpcErr := &RPCError{Code: MethodNotFound, Message: fmt.Sprintf("method not supported: %s", msg.Method)}
	if c.OnRequest != nil {
		result, rpcErr = c.OnRequest(msg.Method, msg.Params)
	}

	var resp any
	if rpcErr != nil {
		resp = errorResponse{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	} else {
		resp = successResponse{JSONRPC: "2.0", ID: msg.ID, Result: result}
	}
	// A write failure means the connection is broken; pending calls will
	// fail when the read loop exits, so there is nothing more to do here.
	_ = c.write(resp)
}

func (c *Client) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.writer, "%s\n", data); err != nil {
		return fmt.Errorf("writing message: %w", err)
	}
	return nil
}

// Call sends a JSON-RPC request and waits for the matching response. If
// result is non-nil, the response result is unmarshaled into it.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	// Register a completion callback; the buffered channel lets the read
	// loop complete the call without blocking.
	responses := make(chan message, 1)
	c.pendingMu.Lock()
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	c.pending[key] = func(msg message) { responses <- msg }
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
	}()

	req := request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := c.write(req); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("connection to agent closed: %w", c.readErr)
	case msg := <-responses:
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, result); err != nil {
				return fmt.Errorf("unmarshaling %s result: %w", method, err)
			}
		}
		return nil
	}
}

// Notify sends a JSON-RPC notification; no response is expected.
func (c *Client) Notify(method string, params any) error {
	return c.write(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// Initialize performs the ACP initialization handshake.
func (c *Client) Initialize(ctx context.Context, req InitializeRequest) (*InitializeResponse, error) {
	var resp InitializeResponse
	if err := c.Call(ctx, "initialize", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Authenticate authenticates with the agent using one of the auth methods
// advertised in the InitializeResponse.
func (c *Client) Authenticate(ctx context.Context, req AuthenticateRequest) error {
	return c.Call(ctx, "authenticate", req, nil)
}

// NewSession creates a new session. A nil MCPServers field is sent as an
// empty list, which the protocol requires.
func (c *Client) NewSession(ctx context.Context, req NewSessionRequest) (*NewSessionResponse, error) {
	if req.MCPServers == nil {
		req.MCPServers = []MCPServer{}
	}
	var resp NewSessionResponse
	if err := c.Call(ctx, "session/new", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// LoadSession resumes a previous session by ID. The agent replays the
// session history as session/update notifications before returning.
func (c *Client) LoadSession(ctx context.Context, req LoadSessionRequest) error {
	if req.MCPServers == nil {
		req.MCPServers = []MCPServer{}
	}
	return c.Call(ctx, "session/load", req, nil)
}

// Prompt sends a user prompt and blocks until the turn completes. Streaming
// output arrives via session/update notifications while the call is pending.
func (c *Client) Prompt(ctx context.Context, req PromptRequest) (*PromptResponse, error) {
	var resp PromptResponse
	if err := c.Call(ctx, "session/prompt", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
