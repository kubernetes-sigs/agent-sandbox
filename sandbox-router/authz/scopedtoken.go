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

package authz

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// headerSandboxPodIP mirrors proxy.HeaderSandboxPodIP. It cannot be
// imported: proxy depends on this package. ScopedTokenAuthorizer
// refuses requests carrying it — the header overrides the dial target
// after authorization, which would let a token scoped to one sandbox
// reach any IP the router can dial.
const headerSandboxPodIP = "X-Sandbox-Pod-Ip"

// MinScopedTokenSecretLen is the minimum accepted secret length, after
// surrounding whitespace is trimmed. Any observed token is an offline
// brute-force oracle for the shared secret, and a short secret makes
// tokens for every sandbox forgeable; 32 bytes matches the
// HMAC-SHA256 output size.
const MinScopedTokenSecretLen = 32

// normalizeScopedSecret trims surrounding whitespace (so a mounted
// Secret with a trailing newline yields the same key for minting and
// verifying), enforces MinScopedTokenSecretLen, and returns a private
// copy so later mutation of the caller's slice cannot change auth
// behavior.
func normalizeScopedSecret(secret []byte) ([]byte, error) {
	s := bytes.TrimSpace(secret)
	if len(s) < MinScopedTokenSecretLen {
		return nil, fmt.Errorf("scopedtoken: secret must be at least %d bytes after trimming whitespace, got %d", MinScopedTokenSecretLen, len(s))
	}
	return append([]byte(nil), s...), nil
}

// scopedClaims is the signed payload of a scoped token: the
// (namespace, name) pair the token is bound to, plus an expiry.
//
// Unlike TokenReviewAuthorizer — which authenticates a principal but
// then lets any authenticated caller reach any sandbox it names (see
// that type's docstring) — a scoped token is bound to exactly one
// sandbox. It is minted by whoever creates the Sandbox (typically the
// controller, standing in for it in this package via
// MintScopedToken) and handed to the agent instead of a
// cluster-verifiable K8s credential. A token minted for sandbox A is
// worthless against sandbox B.
type scopedClaims struct {
	Namespace string `json:"ns"`
	Name      string `json:"name"`
	Exp       int64  `json:"exp"`
}

// MintScopedToken produces a token bound to (namespace, name), signed
// with secret and valid until ttl elapses.
//
// This lives in the router's package for now so the pattern can be
// exercised end-to-end (tests, examples) without a second component.
// The natural home for calling it in production is the Sandbox
// controller at creation time — surfacing the result via the Sandbox
// status or a controller-managed Secret is tracked as a follow-up;
// the router itself never mints tokens, only verifies them.
func MintScopedToken(secret []byte, namespace, name string, ttl time.Duration) (string, error) {
	key, err := normalizeScopedSecret(secret)
	if err != nil {
		return "", err
	}
	if namespace == "" || name == "" {
		return "", errors.New("scopedtoken: namespace and name are required")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("scopedtoken: ttl must be positive, got %s", ttl)
	}
	claims := scopedClaims{Namespace: namespace, Name: name, Exp: time.Now().Add(ttl).Unix()}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("scopedtoken: marshal claims: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	return encPayload + "." + base64.RawURLEncoding.EncodeToString(signScopedToken(key, encPayload)), nil
}

func signScopedToken(secret []byte, encPayload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encPayload))
	return mac.Sum(nil)
}

// ScopedTokenOptions configures a ScopedTokenAuthorizer.
type ScopedTokenOptions struct {
	// Secret is the shared HMAC-SHA256 key used to verify scoped
	// tokens. Required, at least MinScopedTokenSecretLen bytes after
	// whitespace trimming, and must match whatever minted the token
	// (see MintScopedToken; both sides trim identically, so a mounted
	// Secret with a trailing newline still interoperates). Rotate by
	// restarting the router with a new secret; multi-key rotation
	// without downtime is a follow-up (mirrors how TLSCertFile started
	// single-cert before hot-reload was added).
	Secret []byte
	// Clock returns the current time; nil defaults to time.Now. Tests
	// override this to exercise expiry deterministically.
	Clock func() time.Time
}

// ScopedTokenAuthorizer authenticates and authorizes a request in one
// step: it verifies the Bearer token's HMAC signature and expiry, then
// checks the token's (namespace, name) claims against the sandbox the
// request is actually targeting. A verified token for one sandbox is
// rejected with ErrForbidden against any other — the per-sandbox
// scoping TokenReviewAuthorizer explicitly leaves out of its v1 scope.
//
// This gives an agent a single-purpose credential scoped to its own
// sandbox instead of a cluster-verifiable K8s Bearer token, without a
// third-party gateway or vendor runtime image — the property
// examples/containarium-ssh-sandbox demonstrates with an SSH key and
// a forced command, reproduced here with primitives already native to
// agent-sandbox (the router's Authorizer contract on this side; the
// Sandbox controller as the natural minter on the other).
type ScopedTokenAuthorizer struct {
	secret []byte
	clock  func() time.Time
}

// NewScopedTokenAuthorizer builds an authorizer from o.
func NewScopedTokenAuthorizer(o ScopedTokenOptions) (*ScopedTokenAuthorizer, error) {
	key, err := normalizeScopedSecret(o.Secret)
	if err != nil {
		return nil, err
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	return &ScopedTokenAuthorizer{secret: key, clock: clock}, nil
}

// Authorize implements the Authorizer interface.
func (a *ScopedTokenAuthorizer) Authorize(_ context.Context, r *http.Request, sandboxNamespace, sandboxName string) error {
	token, ok := BearerTokenFromRequest(r)
	if !ok {
		return ErrUnauthenticated
	}
	claims, err := a.verify(token)
	if err != nil {
		return ErrUnauthenticated
	}
	// X-Sandbox-Pod-IP replaces the dial target *after* authorization:
	// the proxy would authorize against the claimed (namespace, name)
	// and then dial the caller-supplied IP, so a token scoped to one
	// sandbox could reach anything the router can dial. Scoped tokens
	// therefore never accept the override — route by (namespace, name)
	// only.
	if r.Header.Get(headerSandboxPodIP) != "" {
		return ErrForbidden
	}
	if claims.Namespace != sandboxNamespace || claims.Name != sandboxName {
		return ErrForbidden
	}
	return nil
}

func (a *ScopedTokenAuthorizer) verify(token string) (*scopedClaims, error) {
	encPayload, encSig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, errors.New("scopedtoken: malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return nil, fmt.Errorf("scopedtoken: decode signature: %w", err)
	}
	if !hmac.Equal(sig, signScopedToken(a.secret, encPayload)) {
		return nil, errors.New("scopedtoken: signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return nil, fmt.Errorf("scopedtoken: decode payload: %w", err)
	}
	var claims scopedClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("scopedtoken: unmarshal claims: %w", err)
	}
	// >= : exp is exclusive — a token is invalid from its exp second
	// onward, rather than staying valid for the whole exp second.
	if a.clock().Unix() >= claims.Exp {
		return nil, errors.New("scopedtoken: token expired")
	}
	return &claims, nil
}
