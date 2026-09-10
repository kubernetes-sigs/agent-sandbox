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

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uniqueOwner returns an owner name that can't collide with a concurrent
// test run or a real cached claim. claimCachePath always resolves against
// the real os.TempDir() (there's no injection point), so cache tests need a
// name -- not a directory -- with the same collision-free guarantee
// t.TempDir() gives paths.
func uniqueOwner(t *testing.T) string {
	t.Helper()
	return "test-" + filepath.Base(t.TempDir())
}

func TestClaimCachePath(t *testing.T) {
	got := claimCachePath("alice")
	want := filepath.Join(os.TempDir(), "kata-aks-client-alice.claim")
	if got != want {
		t.Errorf("claimCachePath(%q) = %q, want %q", "alice", got, want)
	}
}

func TestCachedClaimRoundTrip(t *testing.T) {
	owner := uniqueOwner(t)
	t.Cleanup(func() { clearCachedClaim(owner) })

	if got := readCachedClaim(owner); got != "" {
		t.Fatalf("readCachedClaim before write = %q, want empty", got)
	}

	if err := writeCachedClaim(owner, "claim-123"); err != nil {
		t.Fatalf("writeCachedClaim: %v", err)
	}
	if got := readCachedClaim(owner); got != "claim-123" {
		t.Errorf("readCachedClaim = %q, want %q", got, "claim-123")
	}

	clearCachedClaim(owner)
	if got := readCachedClaim(owner); got != "" {
		t.Errorf("readCachedClaim after clear = %q, want empty", got)
	}
}

func TestWriteCachedClaimTrimsTrailingNewlineOnRead(t *testing.T) {
	owner := uniqueOwner(t)
	t.Cleanup(func() { clearCachedClaim(owner) })

	if err := writeCachedClaim(owner, "claim-456"); err != nil {
		t.Fatalf("writeCachedClaim: %v", err)
	}
	// writeCachedClaim appends its own trailing newline; readCachedClaim
	// must strip it back off rather than returning "claim-456\n".
	if got := readCachedClaim(owner); got != "claim-456" {
		t.Errorf("readCachedClaim = %q, want %q (no trailing newline)", got, "claim-456")
	}
}

func TestClearCachedClaimOnMissingFileIsNoop(t *testing.T) {
	owner := uniqueOwner(t)
	clearCachedClaim(owner) // must not panic or error when nothing was ever written
}

func TestChat_Success(t *testing.T) {
	var gotReq *http.Request
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Owner: "alice", Reply: "I am alice's agent.", HistoryTurns: 1,
		})
	}))
	defer server.Close()

	resp, err := chat(context.Background(), server.URL, "sbx-1", "ns-1", "alice", "hello")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Owner != "alice" || resp.Reply != "I am alice's agent." || resp.HistoryTurns != 1 {
		t.Errorf("chat() = %+v, want owner=alice reply set turns=1", resp)
	}

	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/chat" {
		t.Errorf("request = %s %s, want POST /chat", gotReq.Method, gotReq.URL.Path)
	}
	wantHeaders := map[string]string{
		"X-Sandbox-ID":        "sbx-1",
		"X-Sandbox-Namespace": "ns-1",
		"X-Sandbox-Port":      agentPort,
		"X-Owner":             "alice",
	}
	for header, want := range wantHeaders {
		if got := gotReq.Header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}

	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}
	if payload["prompt"] != "hello" {
		t.Errorf("body prompt = %q, want %q", payload["prompt"], "hello")
	}
}

func TestChat_NonSuccessStatusReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream unavailable"))
	}))
	defer server.Close()

	_, err := chat(context.Background(), server.URL, "sbx-1", "ns-1", "alice", "hello")
	if err == nil {
		t.Fatal("chat() = nil error, want an error for a 502 response")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Errorf("err = %q, want it to mention status 502 and the body", err)
	}
}

func TestChat_MalformedJSONReturnsDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	_, err := chat(context.Background(), server.URL, "sbx-1", "ns-1", "alice", "hello")
	if err == nil {
		t.Fatal("chat() = nil error, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode reply") {
		t.Errorf("err = %q, want it to mention the decode failure", err)
	}
}

func TestChat_RequestBodyEncodesPromptRegardlessOfSpecialCharacters(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(chatResponse{Owner: "alice", Reply: "ok"})
	}))
	defer server.Close()

	prompt := `say "hi" and a newline\n`
	if _, err := chat(context.Background(), server.URL, "sbx-1", "ns-1", "alice", prompt); err != nil {
		t.Fatalf("chat: %v", err)
	}

	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("request body is not valid JSON: %v (body=%s)", err, gotBody)
	}
	if payload["prompt"] != prompt {
		t.Errorf("body prompt = %q, want %q", payload["prompt"], prompt)
	}
}
