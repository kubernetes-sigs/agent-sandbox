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
	"net/url"
	"strings"
)

// AuthorizationTarget is the canonical request identity an Authorizer
// evaluates. The proxy constructs it only after routing has selected the
// sandbox and stripped any path-routing prefix, so Path is the path the
// upstream sandbox will receive rather than the router-facing path.
type AuthorizationTarget struct {
	Namespace   string
	SandboxName string
	SandboxUID  string
	Port        int
	Method      string
	Path        string
}

// NormalizeAuthorizationTarget returns the stable representation used in
// scoped-token claims and request comparisons.
func NormalizeAuthorizationTarget(target AuthorizationTarget) (AuthorizationTarget, error) {
	target.Method = strings.ToUpper(strings.TrimSpace(target.Method))
	if !validHTTPMethod(target.Method) {
		return AuthorizationTarget{}, errors.New("authorization target: invalid HTTP method")
	}
	if target.Namespace == "" || target.SandboxName == "" {
		return AuthorizationTarget{}, errors.New("authorization target: namespace and sandbox name are required")
	}
	if target.Port < 1 || target.Port > 65535 {
		return AuthorizationTarget{}, errors.New("authorization target: port must be between 1 and 65535")
	}
	if target.Path == "" {
		target.Path = "/"
	}
	if !strings.HasPrefix(target.Path, "/") {
		return AuthorizationTarget{}, errors.New("authorization target: path must be absolute")
	}
	decodedPath, err := url.PathUnescape(target.Path)
	if err != nil {
		return AuthorizationTarget{}, errors.New("authorization target: path has invalid percent-encoding")
	}
	pathURL := &url.URL{Path: decodedPath, RawPath: target.Path}
	target.Path = uppercasePercentEncoding(pathURL.EscapedPath())
	return target, nil
}

func uppercasePercentEncoding(value string) string {
	encoded := []byte(value)
	for i := 0; i+2 < len(encoded); i++ {
		if encoded[i] != '%' {
			continue
		}
		encoded[i+1] = uppercaseHex(encoded[i+1])
		encoded[i+2] = uppercaseHex(encoded[i+2])
		i += 2
	}
	return string(encoded)
}

func uppercaseHex(value byte) byte {
	if value >= 'a' && value <= 'f' {
		return value - ('a' - 'A')
	}
	return value
}

func validHTTPMethod(method string) bool {
	if method == "" {
		return false
	}
	for i := range len(method) {
		c := method[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

// Authorizer decides whether the principal carried by an inbound
// request may access a particular sandbox. Implementations must be
// safe for concurrent use.
//
// Implementations are responsible for extracting whatever credential
// they need from r (TLS client cert, Bearer token, custom header) and
// turning it into a verified identity — that flow is highly
// implementation-specific (TokenReview, JWT validation, mesh-issued
// SVID, etc.). Helper BearerTokenFromRequest lives in identity.go for
// the common case.
//
// The returned error, when non-nil, should be one of the sentinel
// errors declared in this package so the caller can map it to the
// right HTTP status code.
type Authorizer interface {
	Authorize(ctx context.Context, r *http.Request, target AuthorizationTarget) error
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
func (AllowAll) Authorize(_ context.Context, _ *http.Request, _ AuthorizationTarget) error {
	return nil
}
