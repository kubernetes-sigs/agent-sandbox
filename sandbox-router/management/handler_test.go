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

package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// newTestHandler builds an http.Handler wired to a fake Kubernetes client.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return NewHandler(newTestClient(t), logr.Discard(), "default")
}

func postSandbox(t *testing.T, h http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// --- k8sLabelValue regex ---

func TestIdempotencyKeyRegex(t *testing.T) {
	cases := []struct {
		key   string
		valid bool
	}{
		// Valid: UUID (canonical SDK-generated key)
		{"550e8400-e29b-41d4-a716-446655440000", true},
		// Valid: short alphanumeric
		{"abc", true},
		{"a", true},
		{"a1b2c3", true},
		// Valid: dots and underscores are allowed inside
		{"my.key", true},
		{"my_key", true},
		// Valid: exactly 63 chars
		{strings.Repeat("a", 63), true},
		// Invalid: too long
		{strings.Repeat("a", 64), false},
		// Invalid: leading / trailing hyphen
		{"-bad", false},
		{"bad-", false},
		// Invalid: space
		{"has space", false},
		// Invalid: special characters
		{"key@value", false},
		{"key/value", false},
		{"key:value", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got := k8sLabelValue.MatchString(tc.key)
			if got != tc.valid {
				t.Errorf("k8sLabelValue.MatchString(%q) = %v, want %v", tc.key, got, tc.valid)
			}
		})
	}
}

// --- Handler validation ---

func TestHandlerCreate_MissingWarmPool(t *testing.T) {
	rr := postSandbox(t, newTestHandler(t), map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestHandlerCreate_InvalidIdempotencyKey(t *testing.T) {
	h := newTestHandler(t)
	badKeys := []string{
		strings.Repeat("x", 64), // too long
		"-leading-hyphen",
		"trailing-hyphen-",
		"has space",
		"key@symbol",
		"key/slash",
	}
	for _, key := range badKeys {
		t.Run(key, func(t *testing.T) {
			rr := postSandbox(t, h, map[string]any{
				"warmPool":       "pool-a",
				"idempotencyKey": key,
			})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (key=%q)", rr.Code, http.StatusBadRequest, key)
			}
		})
	}
}

func TestHandlerCreate_ValidKeyAccepted(t *testing.T) {
	rr := postSandbox(t, newTestHandler(t), map[string]any{
		"warmPool":       "pool-a",
		"idempotencyKey": "550e8400-e29b-41d4-a716-446655440000",
	})
	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusAccepted, rr.Body)
	}
}

// --- End-to-end idempotency through the handler ---

func TestHandlerCreate_Idempotent(t *testing.T) {
	h := newTestHandler(t)
	body := map[string]any{
		"warmPool":       "pool-a",
		"idempotencyKey": "550e8400-e29b-41d4-a716-446655440000",
	}

	rr1 := postSandbox(t, h, body)
	if rr1.Code != http.StatusAccepted {
		t.Fatalf("first create: status=%d body=%s", rr1.Code, rr1.Body)
	}
	var resp1 SandboxResponse
	if err := json.NewDecoder(rr1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	rr2 := postSandbox(t, h, body)
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("second create: status=%d body=%s", rr2.Code, rr2.Body)
	}
	var resp2 SandboxResponse
	if err := json.NewDecoder(rr2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if resp1.ID != resp2.ID {
		t.Errorf("idempotency broken: first ID=%q, second ID=%q", resp1.ID, resp2.ID)
	}
}
