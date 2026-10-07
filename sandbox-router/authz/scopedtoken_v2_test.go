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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newScopedTokenV2Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	return publicKey, privateKey
}

func scopedTokenV2Target() AuthorizationTarget {
	return AuthorizationTarget{
		Namespace:   "team-a",
		SandboxName: "box-a",
		SandboxUID:  "4f737e4b-22dc-4a61-a2db-c765fe28f967",
		Port:        9090,
		Method:      "GET",
		Path:        "/exec/run",
	}
}

func TestScopedTokenV2_BindsCanonicalAuthorizationTarget(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	target := scopedTokenV2Target()
	mintTarget := target
	mintTarget.Method = "get"
	token, err := MintScopedTokenV2(privateKey, "current", mintTarget, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if err := authorizer.Authorize(context.Background(), reqWithBearer(token), target); err != nil {
		t.Fatalf("expected canonical target to authorize, got %v", err)
	}
}

func TestScopedTokenV2_RejectsNoncanonicalRequestMethod(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	target := scopedTokenV2Target()
	token, err := MintScopedTokenV2(privateKey, "current", target, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	target.Method = "get"
	request := reqWithBearer(token)
	request.Method = "get"
	if err := authorizer.Authorize(context.Background(), request, target); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected lowercase request method to be forbidden, got %v", err)
	}
}

func TestScopedTokenV2_RejectsEveryMismatchedTargetField(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	target := scopedTokenV2Target()
	token, err := MintScopedTokenV2(privateKey, "current", target, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AuthorizationTarget)
	}{
		{"namespace", func(t *AuthorizationTarget) { t.Namespace = "team-b" }},
		{"sandbox name", func(t *AuthorizationTarget) { t.SandboxName = "box-b" }},
		{"Sandbox UID", func(t *AuthorizationTarget) { t.SandboxUID = "replacement-uid" }},
		{"execution port", func(t *AuthorizationTarget) { t.Port++ }},
		{"HTTP method", func(t *AuthorizationTarget) { t.Method = "POST" }},
		{"upstream path", func(t *AuthorizationTarget) { t.Path = "/exec/other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := target
			test.mutate(&got)
			err := authorizer.Authorize(context.Background(), reqWithBearer(token), got)
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected ErrForbidden, got %v", err)
			}
		})
	}
}

func TestScopedTokenV2_RejectsTokenAtExactExpiry(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	token, err := MintScopedTokenV2(privateKey, "current", scopedTokenV2Target(), time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	claims := decodeScopedTokenV2Claims(t, token)
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
		Clock:            func() time.Time { return time.Unix(claims.Exp, 0) },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := authorizer.Authorize(context.Background(), reqWithBearer(token), scopedTokenV2Target()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated at exact expiry, got %v", err)
	}
}

func TestScopedTokenV2_AcceptsOverlappingVerificationKeys(t *testing.T) {
	oldPublic, oldPrivate := newScopedTokenV2Key(t)
	newPublic, newPrivate := newScopedTokenV2Key(t)
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{
			"old": oldPublic,
			"new": newPublic,
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	for keyID, privateKey := range map[string]ed25519.PrivateKey{"old": oldPrivate, "new": newPrivate} {
		t.Run(keyID, func(t *testing.T) {
			token, err := MintScopedTokenV2(privateKey, keyID, scopedTokenV2Target(), time.Minute)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if err := authorizer.Authorize(context.Background(), reqWithBearer(token), scopedTokenV2Target()); err != nil {
				t.Fatalf("expected %s key to authorize, got %v", keyID, err)
			}
		})
	}
}

func TestScopedTokenV2_RejectsUnknownOrSubstitutedKeyID(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	token, err := MintScopedTokenV2(privateKey, "current", scopedTokenV2Target(), time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey, "next": publicKey},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	for name, changed := range map[string]string{
		"unknown":     strings.Replace(token, "v2.current.", "v2.unknown.", 1),
		"substituted": strings.Replace(token, "v2.current.", "v2.next.", 1),
	} {
		t.Run(name, func(t *testing.T) {
			err := authorizer.Authorize(context.Background(), reqWithBearer(changed), scopedTokenV2Target())
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
		})
	}
}

func TestScopedTokenV2_RejectsDotDelimitedKeyID(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	if _, err := MintScopedTokenV2(privateKey, "2026.08", scopedTokenV2Target(), time.Minute); err == nil {
		t.Fatal("expected dot-delimited key ID to be rejected by minter")
	}
	if _, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"2026.08": publicKey},
	}); err == nil {
		t.Fatal("expected dot-delimited key ID to be rejected by verifier")
	}
}

func TestScopedTokenV2_RejectsSignedTokenWithoutSandboxUID(t *testing.T) {
	publicKey, privateKey := newScopedTokenV2Key(t)
	claims := scopedTokenV2Claims{
		Namespace:   "team-a",
		SandboxName: "box-a",
		Port:        9090,
		Method:      "GET",
		Path:        "/exec/run",
		Exp:         time.Now().Add(time.Minute).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	token := strings.Join([]string{
		scopedTokenV2Version,
		"current",
		encodedPayload,
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, scopedTokenV2SigningInput("current", encodedPayload))),
	}, ".")
	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	target := scopedTokenV2Target()
	target.SandboxUID = ""
	if err := authorizer.Authorize(context.Background(), reqWithBearer(token), target); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected empty-UID token to be unauthenticated, got %v", err)
	}
}

func TestScopedTokenV2_BoundsLegacyV1Drain(t *testing.T) {
	publicKey, _ := newScopedTokenV2Key(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	legacyToken, err := MintScopedToken(secret, "team-a", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint v1: %v", err)
	}
	cutoff := time.Now().Add(time.Minute).Truncate(time.Second)
	target := testAuthorizationTarget("team-a", "box-a")

	authorizer, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret:           secret,
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
		V1AcceptUntil:    cutoff,
		Clock:            func() time.Time { return cutoff.Add(-time.Second) },
	})
	if err != nil {
		t.Fatalf("new overlap authorizer: %v", err)
	}
	if err := authorizer.Authorize(context.Background(), reqWithBearer(legacyToken), target); err != nil {
		t.Fatalf("expected v1 token during drain to authorize, got %v", err)
	}

	authorizer.clock = func() time.Time { return cutoff }
	if err := authorizer.Authorize(context.Background(), reqWithBearer(legacyToken), target); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected v1 token at cutoff to be rejected, got %v", err)
	}
}

func TestNewScopedTokenAuthorizer_RejectsUnboundedV1AndV2Overlap(t *testing.T) {
	publicKey, _ := newScopedTokenV2Key(t)
	_, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret:           []byte("0123456789abcdef0123456789abcdef"),
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	if err == nil || !strings.Contains(err.Error(), "V1AcceptUntil") {
		t.Fatalf("expected bounded v1 overlap error, got %v", err)
	}
}

func TestParseScopedTokenVerificationKeys_AcceptsOverlapKeySet(t *testing.T) {
	oldPublic, _ := newScopedTokenV2Key(t)
	newPublic, _ := newScopedTokenV2Key(t)
	data := []byte(`{"keys":[` +
		`{"kid":"old","publicKey":"` + base64.RawURLEncoding.EncodeToString(oldPublic) + `"},` +
		`{"kid":"new","publicKey":"` + base64.RawURLEncoding.EncodeToString(newPublic) + `"}` +
		`]}`)
	keys, err := ParseScopedTokenVerificationKeys(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 2 || !oldPublic.Equal(keys["old"]) || !newPublic.Equal(keys["new"]) {
		t.Fatalf("parsed keys do not match input")
	}
}

func decodeScopedTokenV2Claims(t *testing.T, token string) scopedTokenV2Claims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		t.Fatalf("expected v2.kid.payload.signature, got %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims scopedTokenV2Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}
