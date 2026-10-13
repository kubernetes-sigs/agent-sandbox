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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// The gateway token: a short-lived HS256 JWT, signed with the secret the
// gateway also holds, naming the run it was minted for.
//
// Minted here with nothing but the standard library, for two reasons. First,
// this example lives in the agent-sandbox Go module and adding a JWT
// dependency to that module for one example would be a poor trade. Second,
// and more usefully: spelling the claims out makes the point of the example
// legible. The only thing that binds this credential to one run is the
// `run_id` claim plus the `jti` the minter keeps — there is no magic.
//
// In production the minter is the runtime that owns the run (for Containarium,
// the daemon's provisionSkillBox, which mints the same token shape); the
// gateway verifies it with the shared secret and never mints anything itself.
type gatewayClaims struct {
	// Attribution the gateway meters and enforces by.
	Tenant   string `json:"tenant"`
	SkillID  string `json:"skill_id,omitempty"`
	Provider string `json:"provider"`

	// The claim this example is about: the identity of the ONE run this
	// credential is good for. It is what makes "revoke everything issued for
	// run X" a well-posed operation.
	RunID string `json:"run_id,omitempty"`

	// Registered claims. `jti` is the handle the minter keeps so it can
	// revoke exactly what it issued, without having to hold (or re-parse)
	// the token itself.
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	ID        string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// gatewayIssuer scopes the token to the model gateway, so a token minted for
// some other purpose against the same secret cannot be replayed at the model
// egress. The gateway checks this.
const gatewayIssuer = "containarium-model-gateway"

// anthropicProvider is the only provider this example ever mints a token
// for — it covers exactly one credential class (see the README, "What this
// example does not do"). mintGatewayToken used to take provider as a
// parameter, but every call site in this package passed this same literal,
// which is exactly what `unparam` exists to catch: a parameter that never
// varies is not a parameter, it is a constant wearing a disguise.
const anthropicProvider = "anthropic"

// mintedToken is a token plus what the minter must keep in order to kill it.
// Holding the jti rather than the token is the point: the revoker never needs
// a copy of the credential it is revoking.
type mintedToken struct {
	Token     string
	JTI       string
	ExpiresAt time.Time
	RunID     string
}

// mintGatewayToken signs a run-scoped gateway token valid for ttl, for
// anthropicProvider — the only provider this example ever mints for.
func mintGatewayToken(secret []byte, tenant, skillID, runID string, ttl time.Duration) (mintedToken, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return mintedToken{}, fmt.Errorf("generate jti: %w", err)
	}
	jti := hex.EncodeToString(raw)

	now := time.Now().Truncate(time.Second)
	exp := now.Add(ttl)
	claims := gatewayClaims{
		Tenant:    tenant,
		SkillID:   skillID,
		Provider:  anthropicProvider,
		RunID:     runID,
		Issuer:    gatewayIssuer,
		Subject:   tenant,
		ID:        jti,
		IssuedAt:  now.Unix(),
		ExpiresAt: exp.Unix(),
	}

	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return mintedToken{}, fmt.Errorf("marshal header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return mintedToken{}, fmt.Errorf("marshal claims: %w", err)
	}

	b64 := base64.RawURLEncoding
	signing := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))

	return mintedToken{
		Token:     signing + "." + b64.EncodeToString(mac.Sum(nil)),
		JTI:       jti,
		ExpiresAt: exp,
		RunID:     runID,
	}, nil
}
