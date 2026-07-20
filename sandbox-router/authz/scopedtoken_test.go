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
	secret := []byte("test-secret")
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
	secret := []byte("test-secret")
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
	secret := []byte("test-secret")
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
	tok, err := MintScopedToken([]byte("secret-a"), "ns", "box", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("secret-b")})
	err = auth.Authorize(context.Background(), reqWithBearer(tok), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_RejectsExpiredToken(t *testing.T) {
	secret := []byte("test-secret")
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
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("s")})
	err := auth.Authorize(context.Background(), reqWithBearer("not-a-real-token"), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestScopedToken_MissingBearerRejected(t *testing.T) {
	auth, _ := NewScopedTokenAuthorizer(ScopedTokenOptions{Secret: []byte("s")})
	err := auth.Authorize(context.Background(), reqWithBearer(""), "ns", "box")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestNewScopedTokenAuthorizer_RequiresSecret(t *testing.T) {
	if _, err := NewScopedTokenAuthorizer(ScopedTokenOptions{}); err == nil {
		t.Fatal("expected error for empty secret")
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
		{"empty namespace", []byte("s"), "", "box", time.Minute},
		{"empty name", []byte("s"), "ns", "", time.Minute},
		{"non-positive ttl", []byte("s"), "ns", "box", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := MintScopedToken(c.secret, c.namespace, c.sandbox, c.ttl); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
