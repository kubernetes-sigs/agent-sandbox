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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/client-go/kubernetes"
)

// Defaults for the TokenReview authorizer. Override via Options.
const (
	defaultTokenReviewTTL      = 30 * time.Second
	defaultTokenReviewCacheLen = 2048
	defaultTokenReviewTimeout  = 3 * time.Second
)

// TokenReviewOptions configures a TokenReviewAuthorizer.
type TokenReviewOptions struct {
	// Client is the Kubernetes client used to issue TokenReview API
	// calls. Required. The router's main.go reuses the client built for
	// the Pod informer cache so callers don't need a second kubeconfig
	// path.
	Client kubernetes.Interface
	// Log is used for debug-level cache hit/miss + denial messages.
	// A zero-value logr.Logger silently discards.
	Log logr.Logger
	// TTL is how long a positive or negative TokenReview result is
	// cached. Zero uses defaultTokenReviewTTL (30s). Shorter values
	// catch revocations sooner at the cost of more apiserver load;
	// longer values cut load but tolerate revoked tokens for longer.
	TTL time.Duration
	// CacheSize is the maximum number of cached token decisions. Once
	// exceeded, the least-recently-used entry is evicted. Zero uses
	// defaultTokenReviewCacheLen.
	CacheSize int
	// RequestTimeout bounds an individual TokenReview API call. Zero
	// uses defaultTokenReviewTimeout (3s). The proxy's per-request
	// deadline still applies on top of this.
	RequestTimeout time.Duration
	// RequireToken, when true, causes requests without a Bearer token
	// in the Authorization header to be rejected with
	// ErrUnauthenticated. When false, tokenless requests are allowed —
	// useful as a transitional mode while clients are being upgraded.
	RequireToken bool
	// Audiences, when non-empty, is sent in the TokenReview spec so the
	// API server verifies the token was minted for one of these
	// audiences (projected ServiceAccount tokens carry an aud claim).
	// Empty means "no audience check at the API server" — the default,
	// matching how K8s itself authenticates kubelet tokens.
	Audiences []string
}

// TokenReviewAuthorizer authenticates requests by submitting their
// Bearer tokens to the cluster's TokenReview API. Decisions are cached
// (TTL + LRU) to keep the authn overhead off the hot path for
// short-burst traffic from a single principal.
//
// What this implementation does NOT do (v1 scope): authorization
// against a per-sandbox identity (e.g. "only the owner of Sandbox X
// may proxy to it"). The token is authenticated; the resulting UserInfo
// is logged; any authenticated caller is then allowed to reach any
// sandbox they name in X-Sandbox-ID. Tightening this requires an
// agreed identity contract on the Sandbox CR (owner label /
// annotation / SAR-style policy) — tracked as a follow-up after
// KEP-NNNN lands.
type TokenReviewAuthorizer struct {
	client    kubernetes.Interface
	log       logr.Logger
	cache     *cache.LRUExpireCache
	ttl       time.Duration
	timeout   time.Duration
	require   bool
	audiences []string
}

// tokenDecision is the cached result of a TokenReview call. Stored by
// SHA-256 hash of the token so raw tokens never sit in memory.
type tokenDecision struct {
	authenticated bool
	user          authenticationv1.UserInfo
	// err is set when the TokenReview API call itself failed (e.g.
	// apiserver down). Cached briefly so we don't hammer a broken
	// apiserver, but with a shorter effective TTL since the failure
	// might be transient.
	err error
}

// NewTokenReviewAuthorizer builds an authorizer from o. Returns an
// error when required fields are missing.
func NewTokenReviewAuthorizer(o TokenReviewOptions) (*TokenReviewAuthorizer, error) {
	if o.Client == nil {
		return nil, errors.New("tokenreview: Client is required")
	}
	// Reject negative values for the three duration / size knobs.
	// config.Config.Validate() already enforces this for flag-driven
	// callers, but TokenReviewOptions is exported and library
	// consumers can construct it directly with arbitrary values —
	// negative inputs here cause silent runtime failures (negative
	// TTL expires cache entries immediately, negative CacheSize
	// panics inside LRU init, negative RequestTimeout produces an
	// already-canceled context and 5xx's every request).
	if o.TTL < 0 {
		return nil, fmt.Errorf("tokenreview: TTL must be non-negative, got %s", o.TTL)
	}
	if o.CacheSize < 0 {
		return nil, fmt.Errorf("tokenreview: CacheSize must be non-negative, got %d", o.CacheSize)
	}
	if o.RequestTimeout < 0 {
		return nil, fmt.Errorf("tokenreview: RequestTimeout must be non-negative, got %s", o.RequestTimeout)
	}
	if o.TTL == 0 {
		o.TTL = defaultTokenReviewTTL
	}
	if o.CacheSize == 0 {
		o.CacheSize = defaultTokenReviewCacheLen
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = defaultTokenReviewTimeout
	}
	return &TokenReviewAuthorizer{
		client:    o.Client,
		log:       o.Log,
		cache:     cache.NewLRUExpireCache(o.CacheSize),
		ttl:       o.TTL,
		timeout:   o.RequestTimeout,
		require:   o.RequireToken,
		audiences: append([]string(nil), o.Audiences...),
	}, nil
}

// Authorize implements the Authorizer interface.
func (a *TokenReviewAuthorizer) Authorize(ctx context.Context, r *http.Request, sandboxNamespace, sandboxName string) error {
	token, ok := BearerTokenFromRequest(r)
	if !ok {
		if a.require {
			a.log.V(1).Info("authz deny: missing Bearer token",
				"sandbox", sandboxName, "namespace", sandboxNamespace)
			return ErrUnauthenticated
		}
		// Token not required and not provided → allow. This matches the
		// "transitional" mode where the router is wired in but clients
		// haven't all been upgraded to send tokens yet.
		return nil
	}

	key := hashToken(token)
	if v, hit := a.cache.Get(key); hit {
		d := v.(*tokenDecision)
		return a.decide(d, sandboxName, sandboxNamespace, true)
	}

	// Bound the TokenReview RPC; the proxy's per-request deadline still
	// applies on top.
	reqCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	tr := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: a.audiences,
		},
	}
	out, err := a.client.AuthenticationV1().TokenReviews().Create(reqCtx, tr, metav1.CreateOptions{})
	d := &tokenDecision{}
	if err != nil {
		// API failure. Cache briefly (1/3 of TTL, min 1s) so a flapping
		// apiserver does not get pummeled — but expire much sooner
		// than a positive decision so a transient error self-heals.
		d.err = err
		ttl := max(a.ttl/3, time.Second)
		a.cache.Add(key, d, ttl)
		a.log.Error(err, "tokenreview API call failed",
			"sandbox", sandboxName, "namespace", sandboxNamespace)
		return fmt.Errorf("tokenreview: %w", err)
	}
	d.authenticated = out.Status.Authenticated
	d.user = out.Status.User
	a.cache.Add(key, d, a.ttl)
	if d.authenticated {
		// Identity is logged ONCE here, at the moment we insert the
		// fresh TokenReview result into the cache — not on every
		// subsequent allow. Per-request decide() then logs only the
		// non-identifying fields (sandbox, namespace, from_cache), so
		// V(1) doesn't repeat Username / UID / Groups on every cache
		// hit. Operators who need the request-level mapping can
		// correlate via the K8s apiserver's TokenReview audit log.
		a.log.V(1).Info("authz: token authenticated",
			"user", d.user.Username,
			"uid", d.user.UID,
			"groups", d.user.Groups,
			"ttl", a.ttl,
		)
	}
	return a.decide(d, sandboxName, sandboxNamespace, false)
}

// decide converts a tokenDecision into an authz error or nil. Logs at
// V(1) so the chatter only shows up when the operator opts in.
func (a *TokenReviewAuthorizer) decide(d *tokenDecision, sandbox, namespace string, fromCache bool) error {
	if d.err != nil {
		return fmt.Errorf("tokenreview (cached): %w", d.err)
	}
	if !d.authenticated {
		a.log.V(1).Info("authz deny: token not authenticated",
			"sandbox", sandbox, "namespace", namespace, "from_cache", fromCache)
		return ErrUnauthenticated
	}
	// No identity fields on the hot path — Username can be an email
	// (OIDC) and Groups can leak tenant/RBAC structure, and even at
	// V(1) we don't want either repeated per request. Identity is
	// logged once at first cache insertion (see Authorize); audit
	// trails belong on a dedicated sink with retention controls.
	a.log.V(1).Info("authz allow",
		"sandbox", sandbox,
		"namespace", namespace,
		"from_cache", fromCache,
	)
	return nil
}

// hashToken returns a hex-encoded SHA-256 digest of the token. The
// cache stores hashes — not raw tokens — so a leak of the in-memory
// cache (e.g. via core dump or memory scrape) does not surrender every
// authenticated session.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
