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

// Single-user CLI for the kata-aks SandboxTemplate.
//
// One invocation provisions (or adopts) exactly one Kata-isolated agent
// sandbox keyed by `-name`, sends one prompt to that sandbox through
// the shared sandbox-router, and (in one-shot mode) tears it down on
// exit. To exercise the "multiple users in parallel" story, run the
// CLI concurrently in separate shells with different `-name` values —
// each invocation lands on its own Kata VM, and the per-template
// NetworkPolicy (where the CNI enforces it) keeps the pods isolated
// from each other.
//
// The agent image baked into the template is owner-agnostic. Identity
// comes from the `X-Owner` header on the incoming request, which is
// folded into the system prompt so the model is told who its user is
// and instructed to start every reply with "I am <owner>'s agent.".
// That gives us a falsifiable end-to-end proof of routing: if Alice ever
// sees a reply that begins with "I am bob's agent", routing is broken.
//
// Usage:
//
//	export ROUTER_BASE_URL=http://<sandbox-router-public-IP>
//
//	# one-shot: claim a fresh sandbox, chat, tear it down
//	go run ./client -name alice -msg "In one sentence, introduce yourself."
//
//	# reuse the same sandbox across runs (claim survives until you delete it):
//	go run ./client -reuse -name ryan -msg "what day is today"
//	go run ./client -reuse -name ryan -msg "and tomorrow?"
//
//	# tear down a reused sandbox:
//	go run ./client -reuse -name ryan -delete
//
// In -reuse mode the claim name is cached at
// /tmp/kata-aks-client-<name>.claim so subsequent invocations adopt
// the same SandboxClaim (and therefore the same Kata pod).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

const (
	warmPoolName = "kata-aks-warmpool"
	namespace    = "sandbox-agent-demo"
	agentPort    = "8080" // FastAPI agent in sandboxtemplate.yaml
)

// httpClient is a dedicated client with an explicit Timeout, shared by
// /chat and /reset. http.DefaultClient is shared process-wide and has
// no Timeout, so a stalled DNS or TCP handshake can hang the CLI
// indefinitely even when the request context has a deadline. The
// 5-minute ceiling is generous enough to cover slow model responses
// (the router itself uses PROXY_TIMEOUT_SECONDS=180 server-side) and
// well under the main 10-minute context budget in main().
var httpClient = &http.Client{Timeout: 5 * time.Minute}

type chatResponse struct {
	Owner        string `json:"owner"`
	Reply        string `json:"reply"`
	HistoryTurns int    `json:"history_turns"`
}

// chat sends one prompt to a specific sandbox through the router and
// returns the agent's response. The router fans out using the X-Sandbox-*
// headers; the agent persona comes from X-Owner, which the router
// forwards untouched.
func chat(ctx context.Context, routerBaseURL, sandboxName, ns, owner, prompt string) (*chatResponse, error) {
	payload, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerBaseURL+"/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sandbox-ID", sandboxName)
	req.Header.Set("X-Sandbox-Namespace", ns)
	req.Header.Set("X-Sandbox-Port", agentPort)
	req.Header.Set("X-Owner", owner)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("router returned %d: %s", resp.StatusCode, body)
	}
	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode reply: %w (body=%s)", err, body)
	}
	return &out, nil
}

// runForUser provisions one sandbox, sends one prompt, asserts the reply
// names the right owner, and tears the sandbox down on exit. The Client's
// DeleteAll is the safety net if this best-effort cleanup is skipped
// (e.g. panic).
func runForUser(ctx context.Context, client *sandbox.Client, routerBaseURL, owner, prompt string) {
	sb, err := client.CreateSandbox(ctx, warmPoolName, namespace)
	if err != nil {
		log.Printf("[%s] create failed: %v", owner, err)
		return
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.DeleteSandbox(cleanupCtx, sb.ClaimName(), namespace)
	}()

	log.Printf("[%s] sandbox ready: claim=%s sandbox=%s pod=%s",
		owner, sb.ClaimName(), sb.SandboxName(), sb.PodName())

	chatAndAssert(ctx, routerBaseURL, sb, owner, prompt)
}

// chatAndAssert sends one prompt and verifies the owner-naming guarantee.
func chatAndAssert(ctx context.Context, routerBaseURL string, sb *sandbox.Sandbox, owner, prompt string) {
	resp, err := chat(ctx, routerBaseURL, sb.SandboxName(), namespace, owner, prompt)
	if err != nil {
		log.Printf("[%s] chat failed: %v", owner, err)
		return
	}
	want := strings.ToLower(fmt.Sprintf("i am %s's agent", owner))
	if !strings.Contains(strings.ToLower(resp.Reply), want) {
		log.Printf("[%s] FAIL: reply does not name owner. owner-in-resp=%q reply=%q",
			owner, resp.Owner, resp.Reply)
		return
	}
	log.Printf("[%s] OK pod=%s turn=%d reply=%q", owner, sb.PodName(), resp.HistoryTurns, resp.Reply)
}

// resetHistory POSTs /reset to wipe the per-owner conversation history on the agent pod.
func resetHistory(ctx context.Context, routerBaseURL string, sb *sandbox.Sandbox, owner string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerBaseURL+"/reset", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Sandbox-ID", sb.SandboxName())
	req.Header.Set("X-Sandbox-Namespace", namespace)
	req.Header.Set("X-Sandbox-Port", agentPort)
	req.Header.Set("X-Owner", owner)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("router returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// claimCachePath returns the local file used to remember the claim name
// for a given owner across -reuse invocations. /tmp is fine for a demo
// client; production callers should track the claim name themselves.
func claimCachePath(owner string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("kata-aks-client-%s.claim", owner))
}

func readCachedClaim(owner string) string {
	b, err := os.ReadFile(claimCachePath(owner))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeCachedClaim(owner, claimName string) error {
	return os.WriteFile(claimCachePath(owner), []byte(claimName+"\n"), 0o600)
}

func clearCachedClaim(owner string) {
	_ = os.Remove(claimCachePath(owner))
}

// runReuse adopts (or creates) a long-lived sandbox keyed by owner name,
// chats with it, and intentionally does NOT delete it on exit.
func runReuse(ctx context.Context, client *sandbox.Client, routerBaseURL, owner, prompt string, deleteOnly, resetOnly bool) {
	cached := readCachedClaim(owner)

	if deleteOnly {
		if cached == "" {
			log.Printf("[%s] no cached claim to delete", owner)
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.DeleteSandbox(cleanupCtx, cached, namespace); err != nil {
			log.Printf("[%s] delete claim=%s failed: %v", owner, cached, err)
		}
		clearCachedClaim(owner)
		log.Printf("[%s] deleted claim=%s", owner, cached)
		return
	}

	var sb *sandbox.Sandbox
	if cached != "" {
		var err error
		sb, err = client.GetSandbox(ctx, cached, namespace)
		if err != nil {
			if errors.Is(err, sandbox.ErrSandboxDeleted) || strings.Contains(err.Error(), "not found") {
				log.Printf("[%s] cached claim=%s is gone, provisioning a new one", owner, cached)
				clearCachedClaim(owner)
				sb = nil
			} else {
				log.Printf("[%s] get cached claim=%s failed: %v", owner, cached, err)
				return
			}
		} else {
			log.Printf("[%s] reusing claim=%s sandbox=%s pod=%s",
				owner, sb.ClaimName(), sb.SandboxName(), sb.PodName())
		}
	}
	if sb == nil {
		var err error
		sb, err = client.CreateSandbox(ctx, warmPoolName, namespace)
		if err != nil {
			log.Printf("[%s] create failed: %v", owner, err)
			return
		}
		if err := writeCachedClaim(owner, sb.ClaimName()); err != nil {
			log.Printf("[%s] warning: could not cache claim name: %v", owner, err)
		}
		log.Printf("[%s] created and cached claim=%s sandbox=%s pod=%s",
			owner, sb.ClaimName(), sb.SandboxName(), sb.PodName())
	}

	if resetOnly {
		if err := resetHistory(ctx, routerBaseURL, sb, owner); err != nil {
			log.Printf("[%s] reset failed: %v", owner, err)
			return
		}
		log.Printf("[%s] history reset on pod=%s", owner, sb.PodName())
		return
	}

	chatAndAssert(ctx, routerBaseURL, sb, owner, prompt)
}

func main() {
	var (
		name    = flag.String("name", "", "owner name (sent as X-Owner; also keys the -reuse cache)")
		msg     = flag.String("msg", "In one sentence, introduce yourself.", "prompt to send")
		reuse   = flag.Bool("reuse", false, "reuse the previously-created sandbox for -name (do not delete on exit)")
		delOnly = flag.Bool("delete", false, "with -reuse: delete the cached sandbox for -name and exit")
		reset   = flag.Bool("reset", false, "with -reuse: wipe the agent's per-owner chat history and exit")
	)
	flag.Parse()

	routerBaseURL := os.Getenv("ROUTER_BASE_URL")
	if routerBaseURL == "" {
		log.Fatal("ROUTER_BASE_URL must be set to the sandbox-router's external URL " +
			"(e.g. http://<sandbox-router-public-IP>)")
	}
	if *name == "" {
		log.Fatal("-name is required")
	}
	if *delOnly && !*reuse {
		log.Fatal("-delete requires -reuse")
	}
	if *reset && !*reuse {
		log.Fatal("-reset requires -reuse")
	}
	if *reset && *delOnly {
		log.Fatal("-reset and -delete are mutually exclusive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := sandbox.NewClient(ctx, sandbox.Options{
		WarmPoolName: warmPoolName,
		Namespace:    namespace,
	})
	if err != nil {
		log.Fatal(err)
	}

	if *reuse {
		// Reuse path: do NOT register a signal-cleanup handler or call
		// DeleteAll on exit — the whole point is for the claim to survive.
		runReuse(ctx, client, routerBaseURL, *name, *msg, *delOnly, *reset)
		return
	}

	// One-shot path: create on the fly, tear down on exit.
	stop := client.EnableAutoCleanup()
	defer client.DeleteAll(ctx)
	defer stop()
	runForUser(ctx, client, routerBaseURL, *name, *msg)
}
