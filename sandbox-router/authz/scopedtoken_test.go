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
	"net/http"
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
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box-a")); err != nil {
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
	err = auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box-b"))
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
	err = auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns-b", "box"))
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
	err = auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_RejectsExpiredToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret: secret,
		Clock:  func() time.Time { return time.Now().Add(time.Hour) },
	})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_RejectsMalformedToken(t *testing.T) {
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("0123456789abcdef0123456789abcdef")})
	err := auth.Authorize(context.Background(), reqWithBearer("not-a-real-token"), testAuthorizationTarget("ns", "box"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_MissingBearerRejected(t *testing.T) {
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("0123456789abcdef0123456789abcdef")})
	err := auth.Authorize(context.Background(), reqWithBearer(""), testAuthorizationTarget("ns", "box"))
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
			err := auth.Authorize(context.Background(), req, testAuthorizationTarget("ns", "box-a"))
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected ErrForbidden with %s override, got %v", tc.header, err)
			}
		})
	}
}

// decodeScopedClaims returns the claims carried by a minted token, so a
// test can pin the clock to the token's own encoded expiry instead of
// recomputing it (which races the second boundary MintScopedToken
// truncates to).
func decodeScopedClaims(t *testing.T, token string) scopedClaims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected version.payload.signature, got %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims scopedClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

// exp is exclusive: the token is already invalid at the exp second, not
// only after it.
func TestScopedToken_RejectsTokenAtExactExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	exp := time.Unix(decodeScopedClaims(t, tok).Exp, 0)
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret: secret,
		Clock:  func() time.Time { return exp },
	})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated at exact expiry, got %v", err)
	}
}

// The shortest accepted ttl must still produce a usable token: exp has
// one-second resolution, so a token minted for one second has to be
// valid at mint time rather than truncating into the past.
func TestScopedToken_ShortestTTLIsNotBornExpired(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box", time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Verify at the token's own mint second: whatever fraction of the
	// second minting landed on, exp is the next second at the earliest.
	minted := time.Unix(decodeScopedClaims(t, tok).Exp-1, 0)
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret: secret,
		Clock:  func() time.Time { return minted },
	})
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box")); err != nil {
		t.Fatalf("expected allow for a 1s token at mint time, got %v", err)
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
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box")); err != nil {
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
	if err := auth.Authorize(context.Background(), reqWithBearer(tok), testAuthorizationTarget("ns", "box")); err != nil {
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
	err = auth.Authorize(context.Background(), reqWithBearer(unversioned), testAuthorizationTarget("ns", "box"))
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
	if err := auth.Authorize(context.Background(), reqWithBearer(forged), testAuthorizationTarget("ns", "box")); !errors.Is(err, ErrUnauthenticated) {
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

// A scoped token presented via a query parameter or a cookie must
// authorize exactly like one presented via the Authorization header —
// this is what lets ScopedTokenAuthorizer work behind
// --path-routing-prefix, where the caller is a browser that cannot set
// a header at all.
func TestScopedToken_AuthorizesFromQueryAndCookie(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, err := NewScopedTokenAuthorizer(ScopedTokenOptions{
		Secret:         secret,
		TokenLocations: TokenLocations{QueryParam: "token", CookieName: "sid"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	t.Run("query", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/?token="+tok, nil)
		if err := auth.Authorize(context.Background(), req, testAuthorizationTarget("ns", "box-a")); err != nil {
			t.Fatalf("expected allow, got %v", err)
		}
	})
	t.Run("cookie", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: tok})
		if err := auth.Authorize(context.Background(), req, testAuthorizationTarget("ns", "box-a")); err != nil {
			t.Fatalf("expected allow, got %v", err)
		}
	})
	// Still scoped: a token for box-a, however it arrives, is forbidden
	// against box-b.
	t.Run("cookie against other sandbox", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: tok})
		err := auth.Authorize(context.Background(), req, testAuthorizationTarget("ns", "box-b"))
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("expected ErrForbidden, got %v", err)
		}
	})
}

// With TokenLocations left at its zero value, a token in the query
// string or a cookie must be ignored exactly as before this field
// existed — a deployment that hasn't opted in is unaffected.
func TestScopedToken_IgnoresQueryAndCookieByDefault(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MintScopedToken(secret, "ns", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: secret})

	req, _ := http.NewRequest("GET", "/?token="+tok, nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: tok})
	err = auth.Authorize(context.Background(), req, testAuthorizationTarget("ns", "box-a"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated with locations disabled, got %v", err)
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
		// exp has one-second resolution: a sub-second ttl would
		// truncate to a token that is already expired at mint time.
		{"sub-second ttl", []byte("0123456789abcdef0123456789abcdef"), "ns", "box", time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := MintScopedToken(c.secret, c.namespace, c.sandbox, c.ttl); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
