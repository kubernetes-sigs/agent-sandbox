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
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// withReactor wires a custom TokenReview reactor onto a fake clientset
// and tracks how many calls have been made.
func withReactor(t *testing.T, react func(action clienttesting.Action) (handled bool, ret runtime.Object, err error)) (*fake.Clientset, *atomic.Int64) {
	t.Helper()
	cs := fake.NewClientset()
	var calls atomic.Int64
	cs.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		calls.Add(1)
		return react(a)
	})
	return cs, &calls
}

func reqWithBearer(token string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	if token != "" {
		r.Header.Set(AuthorizationHeader, "Bearer "+token)
	}
	return r
}

func TestTokenReview_AuthenticatedAllow(t *testing.T) {
	cs, calls := withReactor(t, func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		tr.Status.Authenticated = true
		tr.Status.User = authenticationv1.UserInfo{Username: "u@example.com", UID: "uid-1"}
		return true, tr, nil
	})
	auth, err := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := auth.Authorize(context.Background(), reqWithBearer("t1"), "ns", "s"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 API call, got %d", calls.Load())
	}
}

func TestTokenReview_UnauthenticatedRejected(t *testing.T) {
	cs, _ := withReactor(t, func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		tr.Status.Authenticated = false
		return true, tr, nil
	})
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard()})
	err := auth.Authorize(context.Background(), reqWithBearer("bad-token"), "ns", "s")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestTokenReview_MissingTokenRespectsRequire(t *testing.T) {
	cs, calls := withReactor(t, func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("API should not be called when no Bearer token present")
		return true, nil, nil
	})
	// require=false → allow when token missing
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard(), RequireToken: false})
	if err := auth.Authorize(context.Background(), reqWithBearer(""), "ns", "s"); err != nil {
		t.Fatalf("require=false should allow missing token, got %v", err)
	}
	// require=true → ErrUnauthenticated
	auth, _ = NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard(), RequireToken: true})
	err := auth.Authorize(context.Background(), reqWithBearer(""), "ns", "s")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("require=true should reject missing token, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("missing-token paths must not hit the API; got %d calls", calls.Load())
	}
}

func TestTokenReview_PositiveResultCached(t *testing.T) {
	cs, calls := withReactor(t, func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		tr.Status.Authenticated = true
		tr.Status.User = authenticationv1.UserInfo{Username: "u"}
		return true, tr, nil
	})
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard(), TTL: 30 * time.Second})

	for i := range 5 {
		if err := auth.Authorize(context.Background(), reqWithBearer("same-token"), "ns", "s"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one API call, got %d (cache miss?)", calls.Load())
	}

	// Different token should bypass cache.
	if err := auth.Authorize(context.Background(), reqWithBearer("other-token"), "ns", "s"); err != nil {
		t.Fatalf("%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two API calls after second token, got %d", calls.Load())
	}
}

func TestTokenReview_NegativeResultCached(t *testing.T) {
	cs, calls := withReactor(t, func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		tr.Status.Authenticated = false
		return true, tr, nil
	})
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard(), TTL: 30 * time.Second})

	for i := range 3 {
		if err := auth.Authorize(context.Background(), reqWithBearer("bad"), "ns", "s"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("call %d expected ErrUnauthenticated, got %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one API call; negative results should also be cached. got %d", calls.Load())
	}
}

func TestTokenReview_APIFailureSurfaces(t *testing.T) {
	cs, calls := withReactor(t, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unreachable")
	})
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{Client: cs, Log: logr.Discard()})

	err := auth.Authorize(context.Background(), reqWithBearer("x"), "ns", "s")
	if err == nil {
		t.Fatal("expected error on API failure")
	}
	// Not ErrUnauthenticated or ErrForbidden — the proxy should map to 500.
	if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrForbidden) {
		t.Fatalf("API failure should not look like auth deny, got %v", err)
	}
	// Briefly cached: subsequent call within TTL should NOT re-hit the API.
	_ = auth.Authorize(context.Background(), reqWithBearer("x"), "ns", "s")
	if calls.Load() != 1 {
		t.Fatalf("expected error cached; got %d API calls", calls.Load())
	}
}

func TestTokenReview_RequiresClient(t *testing.T) {
	_, err := NewTokenReviewAuthorizer(TokenReviewOptions{Client: nil})
	if err == nil {
		t.Fatal("expected error when Client is nil")
	}
}

// TestTokenReview_RejectsNegativeInputs guards the library-consumer
// path: config.Config.Validate() already enforces positive TTL and
// CacheSize for flag-driven setup, but TokenReviewOptions is exported
// and direct callers can pass negative values. Each of the three
// duration / size knobs would silently break the runtime if accepted
// (negative TTL → entries expire immediately, negative CacheSize →
// LRU panic, negative RequestTimeout → already-canceled context),
// so the constructor must fail fast.
func TestTokenReview_RejectsNegativeInputs(t *testing.T) {
	cs := fake.NewClientset()
	cases := []struct {
		name string
		opts TokenReviewOptions
	}{
		{"negative TTL", TokenReviewOptions{Client: cs, TTL: -1 * time.Second}},
		{"negative CacheSize", TokenReviewOptions{Client: cs, CacheSize: -1}},
		{"negative RequestTimeout", TokenReviewOptions{Client: cs, RequestTimeout: -1 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTokenReviewAuthorizer(tc.opts)
			if err == nil {
				t.Fatalf("expected error for %s; got nil", tc.name)
			}
		})
	}
}

func TestTokenReview_PassesAudiences(t *testing.T) {
	var seenAudiences []string
	cs, _ := withReactor(t, func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		seenAudiences = append([]string(nil), tr.Spec.Audiences...)
		out := tr.DeepCopy()
		out.Status.Authenticated = true
		return true, out, nil
	})
	auth, _ := NewTokenReviewAuthorizer(TokenReviewOptions{
		Client:    cs,
		Log:       logr.Discard(),
		Audiences: []string{"sandbox-router"},
	})
	if err := auth.Authorize(context.Background(), reqWithBearer("t"), "ns", "s"); err != nil {
		t.Fatalf("%v", err)
	}
	if len(seenAudiences) != 1 || seenAudiences[0] != "sandbox-router" {
		t.Fatalf("apiserver should see configured audiences; got %v", seenAudiences)
	}
}

func TestHashTokenStable(t *testing.T) {
	a := hashToken("abc")
	b := hashToken("abc")
	if a != b {
		t.Fatal("hash must be stable")
	}
	if hashToken("abc") == hashToken("abd") {
		t.Fatal("different tokens must hash differently")
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex should be 64 chars, got %d", len(a))
	}
}
