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
	"errors"
	"fmt"
	"testing"
)

func TestErrorCode(t *testing.T) {
	authErr := &RPCError{Code: AuthRequired, Message: "authentication required"}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "plain error", err: errors.New("connection closed"), want: 0},
		{name: "rpc error", err: authErr, want: AuthRequired},
		{name: "wrapped rpc error", err: fmt.Errorf("creating session: %w", authErr), want: AuthRequired},
		{name: "doubly wrapped", err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", authErr)), want: AuthRequired},
		{name: "method not found", err: &RPCError{Code: MethodNotFound, Message: "no such method"}, want: MethodNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorCode(tc.err); got != tc.want {
				t.Errorf("ErrorCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsAuthRequiredError(t *testing.T) {
	authErr := &RPCError{Code: AuthRequired, Message: "authentication required"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("connection closed"), want: false},
		{name: "auth required", err: authErr, want: true},
		{name: "wrapped auth required", err: fmt.Errorf("session/new: %w", authErr), want: true},
		{name: "other rpc error", err: &RPCError{Code: InternalError, Message: "boom"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthRequiredError(tc.err); got != tc.want {
				t.Errorf("IsAuthRequiredError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
