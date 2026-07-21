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
	"errors"
	"testing"
	"time"
)

func TestScopedToken_ValidTokenAllowsItsOwnSandbox(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, err := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box-a"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

func TestScopedToken_RejectsOtherSandbox(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box-b")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestScopedToken_RejectsOtherNamespace(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns-a", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns-b", "box")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestScopedToken_RejectsWrongSecret(t *testing.T) {
	tok, err := MintScopedToken([]byte("aaaa456789abcdef0123456789abcdef"), "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("bbbb456789abcdef0123456789abcdef")})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_RejectsExpiredToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Millisecond)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret: secret,
		Clock:  func() time.Time { return time.Now().Add(time.Hour) },
	})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_RejectsMalformedToken(t *testing.T) {
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("0123456789abcdef0123456789abcdef")})
	err := auth.Authorize(context.Background(), reqWithBearer("not-a-real-token"), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_MissingBearerRejected(t *testing.T) {
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("0123456789abcdef0123456789abcdef")})
	err := auth.Authorize(context.Background(), reqWithBearer(""), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

// A valid token must not smuggle in a dial-target override:
// X-Sandbox-Pod-IP redirects the proxy's dial after authorization, so
// accepting it would let a token scoped to box-a reach any IP the
// router can dial.
func TestScopedToken_RejectsPodIPOverride(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	req := reqWithBearer(tok)
	req.Header.Set("X-Sandbox-Pod-Ip", "10.0.0.99")
	err = auth.Authorize(context.Background(), req, "ns", "box-a")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden with pod-IP override, got %v", err)
	}
}

// exp is exclusive: the token is already invalid at the exp second, not
// only after it.
func TestScopedToken_RejectsTokenAtExactExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	minted := time.Now()
	tok, err := MintScopedToken(secret, "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret: secret,
		Clock:  func() time.Time { return minted.Add(time.Minute) },
	})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated at exact expiry, got %v", err)
	}
}

// Minter and verifier trim whitespace identically, so a secret read
// from a mounted Secret with a trailing newline interoperates with one
// read without it.
func TestScopedToken_SecretWhitespaceNormalized(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(append(append([]byte(nil), secret...), '\n'), "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box"); err != nil {
		t.Fatalf("expected allow across newline-suffixed secret, got %v", err)
	}
}

// The authorizer must own its key: mutating the caller's slice after
// construction must not change what verifies.
func TestScopedToken_SecretIsCopied(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	callerOwned := append([]byte(nil), secret...)
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: callerOwned})
	for i := range callerOwned {
		callerOwned[i] = 'x'
	}
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box"); err != nil {
		t.Fatalf("expected allow after mutating caller's slice, got %v", err)
	}
}

func TestNewScopedTokenAuthorizer_RequiresSecret(t *testing.T) {
	if _, err := NewScopedTokenAuthorizer(ScopedTokenOptions{}); err == nil {
		t.Fatal("expected error for empty secret")
	}
	if _, err := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("short")}); err == nil {
		t.Fatalf("expected error for secret shorter than %d bytes", MinScopedTokenSecretLen)
	}
}

func TestMintScopedToken_RequiresFields(t *testing.T) {
	cases := []struct {
		name      string
		secret    []byte
		namespace string
		sandbox   string
		ttl       time.Duration
	}{
		{"empty secret", nil, "ns", "box", time.Minute},
		{"short secret", []byte("s"), "ns", "box", time.Minute},
		{"empty namespace", []byte("0123456789abcdef0123456789abcdef"), "", "box", time.Minute},
		{"empty name", []byte("0123456789abcdef0123456789abcdef"), "ns", "", time.Minute},
		{"non-positive ttl", []byte("0123456789abcdef0123456789abcdef"), "ns", "box", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := MintScopedToken(c.secret, c.namespace, c.sandbox, c.ttl); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
