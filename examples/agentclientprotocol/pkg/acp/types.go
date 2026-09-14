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

package acp

import "encoding/json"

// ProtocolVersion is the ACP protocol version this client implements.
const ProtocolVersion = 1

// Methods the agent calls on the client.
const (
	// MethodRequestPermission asks the client to approve or reject a tool call.
	MethodRequestPermission = "session/request_permission"
	// MethodReadTextFile asks the client to read a text file on the agent's behalf.
	MethodReadTextFile = "fs/read_text_file"
	// MethodWriteTextFile asks the client to write a text file on the agent's behalf.
	MethodWriteTextFile = "fs/write_text_file"
)

// MethodSessionUpdate is the notification streamed by the agent during a
// prompt turn or session replay.
const MethodSessionUpdate = "session/update"

// InitializeRequest are the client's parameters for the initialize handshake.
type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *ClientInfo        `json:"clientInfo,omitempty"`
}

// ClientCapabilities declares which agent → client requests this client can
// answer. An agent only sends fs/* requests if the corresponding capability
// is declared.
type ClientCapabilities struct {
	FS       FSCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

// FSCapabilities declares client-side file system support.
type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// ClientInfo identifies the client implementation to the agent.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResponse is the agent's response to initialize.
type InitializeResponse struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	AgentInfo         *AgentInfo     `json:"agentInfo,omitempty"`
	AgentCapabilities map[string]any `json:"agentCapabilities,omitempty"`
	// AuthMethods lists authentication methods accepted by the authenticate
	// call; empty if the agent does not require authentication.
	AuthMethods []AuthMethod `json:"authMethods,omitempty"`
}

// AgentInfo identifies the agent implementation.
type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// AuthMethod describes one way to authenticate with the agent.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AuthenticateRequest are the parameters for the authenticate call.
type AuthenticateRequest struct {
	MethodID string `json:"methodId"`
}

// MCPServer configures an MCP server the agent should connect to for the
// session. This example does not configure any, so only the empty list is
// used; see the ACP specification for the full schema.
type MCPServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// NewSessionRequest are the parameters for session/new.
type NewSessionRequest struct {
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// NewSessionResponse is the agent's response to session/new.
type NewSessionResponse struct {
	SessionID string         `json:"sessionId"`
	Modes     map[string]any `json:"modes,omitempty"`
	Models    map[string]any `json:"models,omitempty"`
}

// LoadSessionRequest are the parameters for session/load.
type LoadSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// ContentBlock is a piece of prompt or message content. Only text content
// is used by this example; the ACP specification also defines image, audio,
// and resource content.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptRequest are the parameters for session/prompt.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResponse is the agent's response to session/prompt, sent once the
// turn has completed.
type PromptResponse struct {
	StopReason string `json:"stopReason"`
}

// Session update kinds carried in SessionUpdate.Kind.
const (
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdatePlan              = "plan"
)

// SessionUpdateNotification is the payload of a session/update notification.
type SessionUpdateNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is a union of the session update variants. The variant is
// discriminated by SessionUpdateKind, which corresponds to the wire field
// "sessionUpdate" and holds one of the Update* constants.
// Content is left raw because its shape differs by variant: a single
// ContentBlock for message and thought chunks, a list of tool call content
// for tool calls.
type SessionUpdate struct {
	SessionUpdateKind string          `json:"sessionUpdate"`
	Content           json.RawMessage `json:"content,omitempty"`

	// Set for tool_call and tool_call_update.
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	ToolKind   string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`

	// Set for plan.
	Entries []PlanEntry `json:"entries,omitempty"`
}

// PlanEntry is one item in an agent's plan update.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
}

// RequestPermissionParams are the parameters of a session/request_permission
// request from the agent: the tool call awaiting approval and the options
// the user may choose from.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  SessionUpdate      `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// Permission option kinds. Options whose kind starts with "allow" approve
// the tool call; "reject" kinds deny it.
const (
	PermissionAllowOnce    = "allow_once"
	PermissionAllowAlways  = "allow_always"
	PermissionRejectOnce   = "reject_once"
	PermissionRejectAlways = "reject_always"
)

// PermissionOption is one choice offered in a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// RequestPermissionResult is the client's response to a permission request.
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// Permission outcomes.
const (
	// PermissionSelected reports that the user chose one of the options.
	PermissionSelected = "selected"
	// PermissionCancelled reports that the prompt turn was cancelled before
	// the user chose an option.
	PermissionCancelled = "cancelled"
)

// PermissionOutcome reports which option the user selected, if any.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// ReadTextFileParams are the parameters of an fs/read_text_file request.
// Line (1-based) and Limit optionally select a range of lines.
type ReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

// ReadTextFileResult is the client's response to fs/read_text_file.
type ReadTextFileResult struct {
	Content string `json:"content"`
}

// WriteTextFileParams are the parameters of an fs/write_text_file request.
type WriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}
