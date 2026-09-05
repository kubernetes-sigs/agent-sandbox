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
	"crypto/ed25519"
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

// untrustedRoutingHeaders mirror proxy header names (proxy depends on
// this package, so they can't be imported). Scoped-token v1 refuses any
// request that carries one because its claims bind neither override.
//
//   - X-Sandbox-Pod-Ip routes straight to a caller-supplied IP.
//   - X-Sandbox-Uid routes via the router's UID→IP cache, so a token
//     for box-a plus box-b's UID (with X-Sandbox-Id still box-a, to
//     pass the claim check) would land on box-b.
//
// Scoped-token v2 binds the UID and permits that header, but continues
// to reject a raw Pod IP.
var untrustedRoutingHeaders = []string{"X-Sandbox-Pod-Ip", "X-Sandbox-Uid"}

// scopedTokenVersion is the token format version. It is both the
// leading component of the wire format (version.payload.signature) and
// part of the signed MAC context (see scopedTokenMACContext). A
// version discriminator lets a future format (key IDs, extra claims,
// different claim semantics) coexist with outstanding v1 tokens during
// a rollout instead of forcing a flag-day: the verifier switches on
// this prefix before doing anything else. Adding it now is free;
// retrofitting it later would invalidate every already-minted token.
const scopedTokenVersion = "v1"

// scopedTokenMACContext domain-separates the HMAC. The MAC is taken
// over this fixed context string followed by the base64 payload, so a
// signature produced here cannot be verified by — nor collide with —
// any other component that happens to HMAC data with the same shared
// Secret. Without it, any protocol that HMAC-SHA256s a base64 blob
// with this key would produce cross-verifiable signatures. The version
// is baked into the context so v1 and a future v2 also can't
// cross-verify.
const scopedTokenMACContext = "agent-sandbox/scoped-token/" + scopedTokenVersion + "."

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
// with secret and valid until ttl elapses. The wire format is
// version.payload.signature (see scopedTokenVersion); the signature is
// domain-separated (see scopedTokenMACContext). Expiry has one-second
// resolution, so ttl must be at least one second and is truncated to
// whole seconds.
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
	// Exp is stored at one-second resolution, so a sub-second ttl would
	// truncate to a token already expired at mint time (exp is
	// exclusive). Reject what the format cannot represent rather than
	// handing back a dead token; a ttl that isn't a whole number of
	// seconds is truncated, so the effective lifetime can be up to one
	// second shorter than asked for.
	if ttl < time.Second {
		return "", fmt.Errorf("scopedtoken: ttl must be at least %s (exp has one-second resolution), got %s", time.Second, ttl)
	}
	claims := scopedClaims{Namespace: namespace, Name: name, Exp: time.Now().Add(ttl).Unix()}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("scopedtoken: marshal claims: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	encSig := base64.RawURLEncoding.EncodeToString(signScopedToken(key, encPayload))
	return scopedTokenVersion + "." + encPayload + "." + encSig, nil
}

func signScopedToken(secret []byte, encPayload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(scopedTokenMACContext))
	mac.Write([]byte(encPayload))
	return mac.Sum(nil)
}

// ScopedTokenOptions configures a ScopedTokenAuthorizer.
type ScopedTokenOptions struct {
	// Secret is the shared HMAC-SHA256 key used to verify legacy v1
	// tokens. Optional when VerificationKeys enables v2. When set, it
	// must be at least MinScopedTokenSecretLen bytes after
	// whitespace trimming, and must match whatever minted the token
	// (see MintScopedToken; both sides trim identically, so a mounted
	// Secret with a trailing newline still interoperates).
	Secret []byte
	// VerificationKeys selects scoped-token v2 verification by protected
	// key ID. Multiple public keys may overlap during a reader-first key
	// rotation. The authorizer copies the map and every key.
	VerificationKeys map[string]ed25519.PublicKey
	// V1AcceptUntil bounds legacy HMAC token verification while v2
	// issuers drain. It is required when VerificationKeys and Secret are
	// configured together. At and after this instant, v1 is rejected.
	V1AcceptUntil time.Time
	// Clock returns the current time; nil defaults to time.Now. Tests
	// override this to exercise expiry deterministically.
	Clock func() time.Time
	// TokenLocations additionally lets Authorize find the token in a
	// URL query parameter or a cookie, beyond the Authorization header
	// it always checks first (see authz.TokenFromRequest). The zero
	// value keeps this authorizer's behavior exactly as it was before
	// this field existed: Authorization header only.
	TokenLocations TokenLocations
}

// ScopedTokenAuthorizer authenticates and authorizes a request in one
// step. V1 verifies an HMAC and binds namespace plus name. V2 verifies
// Ed25519 and binds the complete AuthorizationTarget. A verified token
// for one target is rejected with ErrForbidden against any other.
//
// This gives an agent a single-purpose credential scoped to its own
// sandbox instead of a cluster-verifiable K8s Bearer token, without a
// third-party gateway or vendor runtime image — the property
// examples/containarium-ssh-sandbox demonstrates with an SSH key and
// a forced command, reproduced here with primitives already native to
// agent-sandbox (the router's Authorizer contract on this side; the
// Sandbox controller as the natural minter on the other).
type ScopedTokenAuthorizer struct {
	secret           []byte
	verificationKeys map[string]ed25519.PublicKey
	v1AcceptUntil    time.Time
	clock            func() time.Time
	locs             TokenLocations
}

// NewScopedTokenAuthorizer builds an authorizer from o.
func NewScopedTokenAuthorizer(o ScopedTokenOptions) (*ScopedTokenAuthorizer, error) {
	var secret []byte
	if len(bytes.TrimSpace(o.Secret)) > 0 {
		key, err := normalizeScopedSecret(o.Secret)
		if err != nil {
			return nil, err
		}
		secret = key
	}
	verificationKeys := make(map[string]ed25519.PublicKey, len(o.VerificationKeys))
	for keyID, key := range o.VerificationKeys {
		if !validScopedTokenKeyID(keyID) {
			return nil, fmt.Errorf("scopedtoken: invalid key ID %q", keyID)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("scopedtoken: public key %q must be %d bytes, got %d", keyID, ed25519.PublicKeySize, len(key))
		}
		verificationKeys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	if len(secret) == 0 && len(verificationKeys) == 0 {
		return nil, errors.New("scopedtoken: a v1 secret or v2 verification key is required")
	}
	if len(verificationKeys) > 0 && len(secret) > 0 && o.V1AcceptUntil.IsZero() {
		return nil, errors.New("scopedtoken: V1AcceptUntil is required when v1 and v2 verification are both enabled")
	}
	if !o.V1AcceptUntil.IsZero() && len(secret) == 0 {
		return nil, errors.New("scopedtoken: V1AcceptUntil requires a v1 secret")
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	return &ScopedTokenAuthorizer{
		secret:           secret,
		verificationKeys: verificationKeys,
		v1AcceptUntil:    o.V1AcceptUntil,
		clock:            clock,
		locs:             o.TokenLocations,
	}, nil
}

// Authorize implements the Authorizer interface.
func (a *ScopedTokenAuthorizer) Authorize(_ context.Context, r *http.Request, target AuthorizationTarget) error {
	token, _, ok := TokenFromRequest(r, a.locs)
	if !ok {
		return ErrUnauthenticated
	}
	version, _, _ := strings.Cut(token, ".")
	switch version {
	case scopedTokenVersion:
		claims, err := a.verifyV1(token)
		if err != nil {
			return ErrUnauthenticated
		}
		for _, header := range untrustedRoutingHeaders {
			if r.Header.Get(header) != "" {
				return ErrForbidden
			}
		}
		if claims.Namespace != target.Namespace || claims.Name != target.SandboxName {
			return ErrForbidden
		}
		return nil
	case scopedTokenV2Version:
		claims, err := a.verifyV2(token)
		if err != nil {
			return ErrUnauthenticated
		}
		if r.Header.Get("X-Sandbox-Pod-Ip") != "" {
			return ErrForbidden
		}
		normalized, err := NormalizeAuthorizationTarget(target)
		if err != nil {
			return ErrForbidden
		}
		if normalized.SandboxUID == "" || r == nil || r.Method != normalized.Method {
			return ErrForbidden
		}
		if claims.Namespace != normalized.Namespace ||
			claims.SandboxName != normalized.SandboxName ||
			claims.SandboxUID != normalized.SandboxUID ||
			claims.Port != normalized.Port ||
			claims.Method != normalized.Method ||
			claims.Path != normalized.Path {
			return ErrForbidden
		}
		return nil
	default:
		return ErrUnauthenticated
	}
}

func (a *ScopedTokenAuthorizer) verifyV1(token string) (*scopedClaims, error) {
	if len(a.secret) == 0 {
		return nil, errors.New("scopedtoken: v1 verification is disabled")
	}
	if !a.v1AcceptUntil.IsZero() && !a.clock().Before(a.v1AcceptUntil) {
		return nil, errors.New("scopedtoken: v1 acceptance window expired")
	}
	// Wire format is version.payload.signature. Switch on the version
	// prefix before anything else so a future format can be handled (or
	// cleanly rejected) here without breaking outstanding v1 tokens.
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != scopedTokenVersion {
		return nil, errors.New("scopedtoken: malformed token")
	}
	encPayload, encSig, ok := strings.Cut(rest, ".")
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

func (a *ScopedTokenAuthorizer) verifyV2(token string) (*scopedTokenV2Claims, error) {
	if len(a.verificationKeys) == 0 {
		return nil, errors.New("scopedtoken: v2 verification is disabled")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != scopedTokenV2Version {
		return nil, errors.New("scopedtoken: malformed v2 token")
	}
	key, ok := a.verificationKeys[parts[1]]
	if !ok {
		return nil, errors.New("scopedtoken: unknown key ID")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("scopedtoken: decode v2 signature: %w", err)
	}
	if !ed25519.Verify(key, scopedTokenV2SigningInput(parts[1], parts[2]), signature) {
		return nil, errors.New("scopedtoken: v2 signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("scopedtoken: decode v2 payload: %w", err)
	}
	var claims scopedTokenV2Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("scopedtoken: unmarshal v2 claims: %w", err)
	}
	if claims.SandboxUID == "" {
		return nil, errors.New("scopedtoken: v2 Sandbox UID is required")
	}
	if a.clock().Unix() >= claims.Exp {
		return nil, errors.New("scopedtoken: token expired")
	}
	return &claims, nil
}
