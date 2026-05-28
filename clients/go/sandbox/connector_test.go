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

package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestConnector_DoesNotFollowRedirects(t *testing.T) {
	// 1. Create a target (local internal listener) server
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("TARGET_REACHED"))
	}))
	defer targetServer.Close()

	// 2. Create a malicious redirect server (simulating the sandbox router)
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetServer.URL, http.StatusFound)
	}))
	defer redirectServer.Close()

	// 3. Setup connector config
	cfg := connectorConfig{
		Strategy:          &DirectStrategy{URL: redirectServer.URL},
		Namespace:         "default",
		ServerPort:        8080,
		RequestTimeout:    5 * time.Second,
		PerAttemptTimeout: 2 * time.Second,
		Log:               logr.Discard(),
	}
	conn := newConnector(cfg)
	err := conn.Connect(context.Background())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	// 4. Send request via connector
	resp, err := conn.SendRequest(context.Background(), http.MethodGet, "/some-endpoint", nil, "", 1)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	defer resp.Body.Close()

	// 5. Check if it followed redirect to the target server
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect response without following it; got status %d", resp.StatusCode)
	}

	if location := resp.Header.Get("Location"); location != targetServer.URL {
		t.Errorf("expected redirect Location %q, got %q", targetServer.URL, location)
	}
}
