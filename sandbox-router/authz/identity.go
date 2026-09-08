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
	"net/http"
	"strings"
)

// AuthorizationHeader is the HTTP header carrying a Bearer token. The
// constant is exported so the proxy and tests share a single source of
// truth.
const AuthorizationHeader = "Authorization"

// BearerSchemePrefix is the case-insensitive prefix that introduces a
// Bearer token in the Authorization header.
const BearerSchemePrefix = "Bearer "

// BearerTokenFromRequest extracts a Bearer token from the Authorization
// header. Returns ("", false) when the header is missing or does not
// start with the case-insensitive "Bearer " prefix.
//
// The scheme match is case-insensitive per RFC 7235 §2.1 ("scheme
// names are matched case-insensitively") but the token itself is
// returned verbatim — tokens are case-sensitive.
func BearerTokenFromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	h := r.Header.Get(AuthorizationHeader)
	if len(h) < len(BearerSchemePrefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(BearerSchemePrefix)], BearerSchemePrefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(BearerSchemePrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// TokenSource identifies which part of the request TokenFromRequest
// found a credential in.
type TokenSource string

const (
	// TokenSourceHeader means the credential came from the Authorization
	// header — the only source BearerTokenFromRequest ever looks at.
	TokenSourceHeader TokenSource = "header"
	// TokenSourceQuery means the credential came from a URL query
	// parameter. This exists solely to bootstrap TokenSourceCookie (see
	// the router's "Browser-session credentials" README section, and
	// proxy.Handler's browser-session bootstrap): a browser cannot set
	// a request header at all for a top-level navigation, an
	// <iframe src>, or a WebSocket handshake, so the very first request
	// has to carry the credential somewhere a browser *can* put it —
	// the URL — before a cookie exists to carry it afterward.
	TokenSourceQuery TokenSource = "query"
	// TokenSourceCookie means the credential came from a cookie. Unlike
	// the header and query sources, a cookie is sent automatically by
	// the browser on every subsequent request to the same origin,
	// including a WebSocket handshake — which is what makes it the only
	// credential source that actually works for a browser-facing
	// deployment. It is also the only source an attacker's page can
	// piggyback on (see the Origin-allowlist check this package's
	// callers are expected to apply whenever the source is a cookie).
	TokenSourceCookie TokenSource = "cookie"
)

// TokenLocations configures where TokenFromRequest may additionally look
// for a credential, beyond the Authorization header that it always
// checks first. Both fields default to "" (disabled): the zero value
// makes TokenFromRequest behave exactly like BearerTokenFromRequest,
// so enabling neither leaves every existing Authorizer's behavior
// byte-for-byte unchanged.
type TokenLocations struct {
	// QueryParam, when non-empty, is the name of a URL query parameter
	// TokenFromRequest treats as carrying the credential.
	QueryParam string
	// CookieName, when non-empty, is the name of a cookie
	// TokenFromRequest treats as carrying the credential.
	CookieName string
}

// TokenFromRequest extracts a credential from r. It checks, in order:
// the Authorization header (see BearerTokenFromRequest), then — only if
// loc enables them — a URL query parameter, then a cookie. It returns
// the credential, which location it came from, and whether one was
// found at all.
//
// The header is always checked first, regardless of loc: a
// server-to-server caller that sends Authorization keeps working
// unchanged no matter what a browser-facing deployment additionally
// enables. Query takes precedence over cookie so a freshly minted token
// in the URL can supersede a stale or missing cookie in the very same
// request — the browser-session bootstrap in package proxy relies on
// this to authorize the request that mints the cookie in the first
// place.
func TokenFromRequest(r *http.Request, loc TokenLocations) (string, TokenSource, bool) {
	if tok, ok := BearerTokenFromRequest(r); ok {
		return tok, TokenSourceHeader, true
	}
	if r == nil {
		return "", "", false
	}
	if loc.QueryParam != "" {
		if tok := r.URL.Query().Get(loc.QueryParam); tok != "" {
			return tok, TokenSourceQuery, true
		}
	}
	if loc.CookieName != "" {
		if c, err := r.Cookie(loc.CookieName); err == nil && c.Value != "" {
			return c.Value, TokenSourceCookie, true
		}
	}
	return "", "", false
}
