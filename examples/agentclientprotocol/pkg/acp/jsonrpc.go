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

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSON-RPC 2.0 wire types. These are internal; callers interact with the
// typed ACP methods on Client and the handler callbacks.

// request is an outgoing JSON-RPC request.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// notification is an outgoing JSON-RPC notification (a request without an
// ID, expecting no response).
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// message is any incoming JSON-RPC message: a response to one of our
// requests (Result/Error), a request from the agent (Method with ID), or a
// notification from the agent (Method without ID).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// successResponse is an outgoing reply to an agent-initiated request.
// The result member is intentionally not omitempty: JSON-RPC requires it to
// be present (possibly null) on success.
type successResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

// errorResponse is an outgoing error reply to an agent-initiated request.
type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC 2.0 error object. It is returned from Call (and
// the typed methods built on it) when the agent responds with an error, and
// may be returned from a RequestHandler to send an error back to the agent.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("RPC error [%d]: %s (data: %s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("RPC error [%d]: %s", e.Code, e.Message)
}

// JSON-RPC 2.0 error codes.
const (
	// MethodNotFound indicates the requested method is not supported.
	MethodNotFound = -32601
	// InvalidParams indicates the request parameters could not be parsed.
	InvalidParams = -32602
	// InternalError indicates the request failed while being handled.
	InternalError = -32603

	// AuthRequired is the ACP error code reporting that the client must
	// authenticate before the request can succeed.
	AuthRequired = -32000
)

// ErrorCode returns the JSON-RPC error code carried by err (looking through
// wrapped errors), or 0 if err does not carry an *RPCError.
func ErrorCode(err error) int {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	return 0
}

// IsAuthRequiredError reports whether err is the agent telling the client
// that it must call authenticate before the request can succeed.
func IsAuthRequiredError(err error) bool {
	return ErrorCode(err) == AuthRequired
}
