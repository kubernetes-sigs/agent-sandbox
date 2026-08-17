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

// Package authz defines the per-request authorization contract for the
// sandbox-router and the small built-in implementations: AllowAll (the
// default no-op) and TokenReview-based (KEP-NNNN compliant; see
// tokenreview.go).
//
// The proxy looks up an Authorizer once at startup and calls Authorize
// for every request after header parsing. A nil error means "allow"; a
// non-nil error means "deny" and is converted to a 401/403 JSON error
// response by the caller.
package authz

import (
	"context"
	"errors"
	"net/http"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// Identity is the authenticated principal extracted from a request. The
// router does not invent identities — it pulls one from a TLS client
// cert (mTLS deployments) or from a Bearer token (TokenReview
// deployments) and hands the struct to the Authorizer. An Identity with
// Source=="" represents an unauthenticated caller; whether that is
// acceptable is the Authorizer's call.
//
// The fields mirror authenticationv1.UserInfo so callers that delegate
// to TokenReview can pass the SubjectAccessReview body through
// unchanged.
type Identity struct {
	// Username is the principal name. For TLS clients this is the
	// certificate Subject CN (or first SPIFFE URI / DNS SAN, in that
	// order of preference); for Bearer tokens it is the value reported
	// by TokenReview.
	Username string
	// UID is the durable identifier returned by TokenReview, when
	// available. Empty for TLS-derived identities.
	UID string
	// Groups are the authenticated groups for this principal.
	Groups []string
	// Extra carries provider-specific attributes (e.g. K8s authenticator
	// extras). May be nil.
	Extra map[string][]string
	// Source records how the identity was derived, for logging and
	// authorizer dispatch. One of "tls", "bearer-token", or "" (unset).
	Source string
}

// IsAuthenticated reports whether the identity carries enough
// information for an Authorizer to reason about it.
func (i Identity) IsAuthenticated() bool {
	return i.Username != "" || i.UID != "" || len(i.Groups) > 0
}

// Authorizer decides whether the principal carried by an inbound
// request may access a particular sandbox. Implementations must be
// safe for concurrent use.
//
// Implementations are responsible for extracting whatever credential
// they need from r (TLS client cert, Bearer token, custom header) and
// turning it into a verified identity — that flow is highly
// implementation-specific (TokenReview, JWT validation, mesh-issued
// SVID, etc.). Helpers IdentityFromTLS and BearerTokenFromRequest live
// in identity.go for the common cases.
//
// The returned error, when non-nil, should be one of the sentinel
// errors declared in this package so the caller can map it to the
// right HTTP status code.
type Authorizer interface {
	Authorize(ctx context.Context, r *http.Request, sandboxNamespace, sandboxName string) error
}

// Sentinel errors returned by Authorizer implementations. The proxy
// maps Unauthenticated → 401 and Forbidden → 403; any other error is
// treated as an internal failure and surfaces as 500.
var (
	// ErrUnauthenticated means no credential was presented or the
	// credential failed verification. Map to HTTP 401.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrForbidden means the identity was verified but is not allowed
	// to access the requested sandbox. Map to HTTP 403.
	ErrForbidden = errors.New("forbidden")
)

// HTTPStatusFor maps an Authorizer error to the HTTP status code that
// should be returned to the client. Unknown errors map to 500 so a bug
// in an Authorizer doesn't silently leak as 403.
func HTTPStatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// AllowAll is the default Authorizer: every request is permitted
// regardless of identity. It is appropriate for development clusters
// and for deployments that handle authorization in a layer in front of
// the router (Envoy, Gateway, mesh policy).
type AllowAll struct{}

// Authorize always returns nil.
func (AllowAll) Authorize(_ context.Context, _ *http.Request, _, _ string) error {
	return nil
}

// FromUserInfo builds an Identity from a UserInfo as returned by the
// authentication.k8s.io/v1 TokenReview API. Provided so the
// TokenReview-based authorizer in tokenreview.go and any other
// authorizer that talks to K8s authn can produce a consistent log shape.
func FromUserInfo(u authenticationv1.UserInfo, source string) Identity {
	id := Identity{
		Username: u.Username,
		UID:      u.UID,
		Groups:   append([]string(nil), u.Groups...),
		Source:   source,
	}
	if len(u.Extra) > 0 {
		id.Extra = make(map[string][]string, len(u.Extra))
		for k, v := range u.Extra {
			id.Extra[k] = append([]string(nil), v...)
		}
	}
	return id
}
