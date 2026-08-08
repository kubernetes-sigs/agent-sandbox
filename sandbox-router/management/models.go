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

// Package management implements the /v1/ SandboxClaim REST management API.
package management

import "time"

// CreateSandboxRequest is the body for POST /v1/sandboxes.
type CreateSandboxRequest struct {
	WarmPool       string            `json:"warmPool"`                 // required; SandboxWarmPool name
	Namespace      string            `json:"namespace,omitempty"`      // defaults to handler's DefaultNamespace
	TTLSeconds     *int32            `json:"ttlSeconds,omitempty"`
	Env            []EnvVar          `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"` // client-generated UUID; safe to retry POST with same key
}

// EnvVar is a key/value environment variable with an optional container target.
type EnvVar struct {
	Name          string `json:"name"`
	Value         string `json:"value"`
	ContainerName string `json:"containerName,omitempty"`
}

// SandboxResponse is returned for create / get operations.
type SandboxResponse struct {
	ID          string          `json:"id"`                    // SandboxClaim name
	Namespace   string          `json:"namespace"`
	SandboxName string          `json:"sandboxName,omitempty"` // assigned Sandbox CR name; empty until Ready
	Status      string          `json:"status"`                // "Pending" / "Ready" / "Failed" / "Expired"
	Ready       bool            `json:"ready"`
	CreatedAt   time.Time       `json:"createdAt"`
	Connection  *ConnectionInfo `json:"connection,omitempty"` // populated when Ready
	Conditions  []ConditionInfo `json:"conditions,omitempty"`
}

// ConnectionInfo tells the caller exactly which headers to set when sending
// requests through the proxy. This removes any SDK knowledge of header names.
type ConnectionInfo struct {
	SandboxID        string            `json:"sandboxId"`
	SandboxNamespace string            `json:"sandboxNamespace"`
	Headers          map[string]string `json:"headers"` // e.g. {"X-Sandbox-ID": "...", "X-Sandbox-Namespace": "..."}
}

// ConditionInfo is a JSON-friendly representation of a metav1.Condition.
type ConditionInfo struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// SandboxListResponse is returned for GET /v1/sandboxes.
type SandboxListResponse struct {
	Items []SandboxResponse `json:"items"`
	Total int               `json:"total"`
}

// ErrorResponse is the JSON body for 4xx / 5xx responses.
type ErrorResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}
