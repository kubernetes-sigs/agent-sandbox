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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
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

// A valid token must not smuggle in a dial-target override: these
// headers redirect the proxy's dial after authorization, so accepting
// one would let a token scoped to box-a reach a different pod even
// though X-Sandbox-Id still says box-a and the claim check passes.
func TestScopedToken_RejectsRoutingOverrides(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	for _, tc := range []struct{ header, value string }{
		{"X-Sandbox-Pod-Ip", "10.0.0.99"},
		{"X-Sandbox-Uid", "some-other-sandbox-uid"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			req := reqWithBearer(tok)
			req.Header.Set(tc.header, tc.value)
			err := auth.Authorize(context.Background(), req, "ns", "box-a")
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected ErrForbidden with %s override, got %v", tc.header, err)
			}
		})
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

// The wire format carries a leading version discriminator
// (version.payload.signature) so a future format can coexist with
// outstanding v1 tokens. Pin it so the format isn't changed silently.
func TestScopedToken_TokenCarriesVersionPrefix(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(tok, "v1.") {
		t.Fatalf("expected token to start with %q, got %q", "v1.", tok)
	}
	if got := strings.Count(tok, "."); got != 2 {
		t.Fatalf("expected version.payload.signature (2 dots), got %d in %q", got, tok)
	}
}

// A token without the version prefix (the pre-v1 two-part format) must
// be rejected rather than silently accepted, so the discriminator
// actually gates verification.
func TestScopedToken_RejectsUnversionedToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	unversioned := strings.TrimPrefix(tok, "v1.")
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	err = auth.Authorize(context.Background(), reqWithBearer(unversioned), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for unversioned token, got %v", err)
	}
}

// Domain separation: the MAC is taken over a fixed context string plus
// the payload, so a bare HMAC-SHA256 of the payload with the same
// secret — what another protocol reusing the key would produce — is not
// a valid signature here.
func TestScopedToken_MACIsDomainSeparated(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	key, err := normalizeScopedSecret(secret)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	claims := scopedClaims{Namespace: "ns", Name: "box", Exp: time.Now().Add(time.Minute).Unix()}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encPayload)) // no context prefix
	bareSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	forged := "v1." + encPayload + "." + bareSig

	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})
	if err := auth.Authorize(context.Background(), reqWithBearer(forged), "ns", "box"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for context-less signature, got %v", err)
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
