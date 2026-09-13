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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// modelCallBody is a minimal, cheap provider request. Its content does not
// matter: every check in this example is about whether the gateway let the
// call through, not about what the model said.
const modelCallBody = `{"model":"claude-3-5-haiku-latest","max_tokens":16,` +
	`"messages":[{"role":"user","content":"ping"}]}`

// modelCallPath is the gateway's data plane: /v1/model/<provider>/<upstream
// path>. The gateway strips the prefix, verifies the caller's token, injects
// the real provider key, and forwards the rest to the provider.
const modelCallPath = "/v1/model/anthropic/v1/messages"

// httpResult is one HTTP exchange, reduced to what the checks assert on.
type httpResult struct {
	Status int
	Body   string
}

// summary renders a result compactly for the log: status plus the first line
// of the body, which is where both the gateway's plain-text rejections and
// the provider's JSON error land.
func (r httpResult) summary() string {
	body := strings.TrimSpace(r.Body)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	const maxLen = 220
	if len(body) > maxLen {
		body = body[:maxLen] + "…"
	}
	return fmt.Sprintf("HTTP %d — %s", r.Status, body)
}

// gatewayRejections are the gateway's OWN refusals, verbatim from the OSS
// gateway handler. They are the discriminator every check in this example
// turns on, so they are listed explicitly rather than pattern-matched:
//
//   - if the response is one of these, the gateway refused the credential and
//     no provider key was ever touched;
//   - if it is anything else, the gateway accepted the credential and the
//     response came from the provider — including the provider's own 401 when
//     the demo runs without a real provider key, which is why "accepted" can
//     NOT simply be read off the status code.
var gatewayRejections = []string{
	"missing gateway token",
	"invalid gateway token",
	"token not valid for provider",
	"gateway token revoked",
	"gateway holds no key for provider",
}

// gatewayRefusal returns the gateway's own refusal text if this response is
// one, and "" if the gateway let the call through to the provider.
func gatewayRefusal(r httpResult) string {
	for _, s := range gatewayRejections {
		if strings.Contains(r.Body, s) {
			return s
		}
	}
	return ""
}

// callModelThroughGateway makes one model call with the given token. Used by
// the runner for the post-exit checks; the in-sandbox equivalent is the probe
// script in probe.go, which speaks the same request from inside the box.
func callModelThroughGateway(ctx context.Context, baseURL, token string) (httpResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+modelCallPath, strings.NewReader(modelCallBody))
	if err != nil {
		return httpResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// The box presents the scoped gateway token where a provider SDK would
	// put an API key. It never holds the key itself.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{Status: resp.StatusCode, Body: string(body)}, nil
}

// revokeRequest is the wire shape of POST /__gateway/revoke.
//
// expires_at is optional and means "this revocation entry may be forgotten
// after that instant". Passing the token's real expiry, as here, lets the
// gateway garbage-collect the entry once the token could not have been
// accepted anyway. Omit it if you do NOT know the real expiry: an empty value
// means never forget, which is the safe direction — a guess that is too short
// silently un-revokes the token.
type revokeRequest struct {
	JTI       string `json:"jti"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// revokeToken ends the run's credential. This is the step whose absence is the
// whole bug: removing a token from a process's environment does not
// invalidate a copy that already left the process.
func revokeToken(ctx context.Context, baseURL, adminToken string, t mintedToken, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(revokeRequest{
		JTI:       t.JTI,
		ExpiresAt: t.ExpiresAt.UTC().Format(time.RFC3339),
		Reason:    reason,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/__gateway/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("revoke %s: gateway returned %d: %s",
			t.JTI, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
