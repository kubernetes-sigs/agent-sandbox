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

package proxy

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// cookieLocations derives the authz.TokenLocations the configured
// Authorizer was (or should have been) built with from cfg. The zero
// value — returned whenever AuthzCookieName is unset, the default —
// makes authz.TokenFromRequest behave exactly like
// authz.BearerTokenFromRequest, so a deployment that hasn't opted into
// this feature is unaffected by anything in this file.
func cookieLocations(cfg *config.Config) authz.TokenLocations {
	if cfg.AuthzCookieName == "" {
		return authz.TokenLocations{}
	}
	return authz.TokenLocations{
		QueryParam: cfg.AuthzCookieQueryParam,
		CookieName: cfg.AuthzCookieName,
	}
}

// credentialSource reports which part of r a credential was found in,
// using the same locations the configured Authorizer checks. It exists
// so ServeHTTP can apply the cookie-only Origin-allowlist check (see
// isAllowedOrigin) BEFORE calling Authorize, without changing the
// authz.Authorizer interface — which authorizes or denies, but never
// reports where the credential it used came from.
func (h *Handler) credentialSource(r *http.Request) authz.TokenSource {
	_, src, ok := authz.TokenFromRequest(r, cookieLocations(h.cfg))
	if !ok {
		return ""
	}
	return src
}

// requestOrigin builds the router's own canonical origin for r, in the
// same scheme://host shape a browser's Origin header uses.
//
// Scheme is derived from whether the connection this request arrived on
// is TLS (r.TLS != nil for the router's own --https-bind-address
// listener) — unless trustForwardedProto is set, in which case a
// present X-Forwarded-Proto is trusted instead. r.TLS alone is wrong
// behind any TLS-terminating load balancer or Gateway, the common
// production shape: the connection this process sees is always plain
// HTTP, so selfOrigin would always compute as http://<host> while a
// browser serving the page over HTTPS sends "Origin: https://<host>" —
// a same-origin request misclassified as cross-origin. trustForwardedProto
// is opt-in (see Config.AuthzTrustForwardedProto) precisely because
// trusting a client-supplied header on faith is only safe when the
// caller has confirmed the router is reachable exclusively through a
// proxy that sets it, stripping any value a client might have sent.
func requestOrigin(r *http.Request, trustForwardedProto bool) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if trustForwardedProto {
		if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
			// A chain of proxies may each append their own value,
			// comma-separated; the first one is what the client-facing
			// edge — the one terminating TLS — actually saw.
			first, _, _ := strings.Cut(fwd, ",")
			if s := strings.ToLower(strings.TrimSpace(first)); s != "" {
				scheme = s
			}
		}
	}
	return scheme + "://" + r.Host
}

// isAllowedOrigin reports whether origin — the raw Origin header value,
// possibly empty — may carry a cookie-sourced credential toward
// selfOrigin (this router's own canonical origin, from requestOrigin)
// under allowed.
//
// An empty origin is let through: there is nothing here for this check
// to inspect, and browsers reliably send Origin on exactly the requests
// this check exists to gate (every WebSocket handshake, and any
// cross-site fetch/XHR/form submission) — a same-origin plain GET
// navigation is the common case that omits it, and that case needs no
// gating in the first place.
//
// A request whose Origin exactly matches selfOrigin — scheme AND host —
// is always allowed regardless of the allowlist; only a genuinely
// different Origin is checked against it. The match is NOT host-only:
// http://host and https://host are different origins, and treating
// them as interchangeable would let a plain-HTTP origin ride a
// Secure, SameSite=None cookie meant only for the HTTPS side of a
// deployment that (unusually, but the config allows it) serves both
// schemes on the same host.
//
// Both sides are compared after normalizeOrigin: a real browser never
// includes a scheme's default port in the Origin header it sends, so
// an allowlist entry (or a --https-bind-address serving on :443)
// written with an explicit ":443"/":80" would otherwise never match
// anything a browser actually sends.
func isAllowedOrigin(origin, selfOrigin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	normOrigin := normalizeOrigin(origin)
	if strings.EqualFold(normOrigin, normalizeOrigin(selfOrigin)) {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(normalizeOrigin(a), normOrigin) {
			return true
		}
	}
	return false
}

// normalizeOrigin strips a scheme's default port (":80" for http,
// ":443" for https) from a scheme://host[:port] origin string, so
// "https://example.com" and "https://example.com:443" compare equal.
// Returns o unchanged if it doesn't parse as a URL at all — callers
// still get a deterministic (non-matching, since a malformed string
// won't equal a well-formed one) comparison rather than a panic.
func normalizeOrigin(o string) string {
	u, err := url.Parse(o)
	if err != nil {
		return o
	}
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		return u.Scheme + "://" + u.Hostname()
	}
	return o
}

// maybeBootstrapCookie implements the browser-session bootstrap: a
// GET/HEAD, non-upgrade request whose credential was found in the URL
// query parameter (Config.AuthzCookieQueryParam) — already proven valid
// by the caller's successful Authorize call — gets that credential set
// as a cookie scoped to exactly this sandbox, and is redirected to the
// same URL with the parameter stripped. It reports whether it wrote a
// response; when true, the caller must not proxy the request any
// further.
//
// This exists because a browser cannot set a request header at all for
// a top-level navigation, an <iframe src>, or a WebSocket handshake —
// the query parameter is the only place the FIRST such request can
// carry a credential. Every request after this one relies on the
// cookie instead, which a browser attaches automatically to any request
// under the cookie's Path, including a WebSocket handshake.
//
// Redirecting immediately, rather than also serving the original
// request's content, is deliberate: it collapses the window during
// which the credential sits in the URL to a single request, so it never
// lands in browser history or in a Referer header a subsequently loaded
// sub-resource might send.
//
// pathRouted must be true only when resolveTarget actually matched
// r against --path-routing-prefix. Without that gate, a header-routed
// request that happens to carry the bootstrap query parameter (nothing
// stops a header-based SDK caller from also setting it, deliberately or
// not) would get redirected to a cookie whose Path can never match a
// header-routed URL — stripping the only credential the retried
// request had, with no way to get it back.
func (h *Handler) maybeBootstrapCookie(w http.ResponseWriter, r *http.Request, target Target, credSrc authz.TokenSource, upgrade, pathRouted bool) bool {
	if h.cfg.AuthzCookieName == "" || credSrc != authz.TokenSourceQuery || !pathRouted {
		return false
	}
	if upgrade {
		// A WebSocket handshake is technically a GET, but a browser's
		// WebSocket constructor does not follow redirects, so it cannot
		// be bootstrapped this way — it must already be relying on a
		// cookie set by an earlier plain-HTTP bootstrap on the same
		// page. Nothing in a real browser flow presents a query-sourced
		// credential on an upgrade request; if one somehow does, it is
		// let through (Authorize already ran) without a cookie.
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// Redirecting away from a request that might carry a body (a
		// POST, say) would silently drop it. Nothing in a real browser
		// flow presents the bootstrap parameter on such a request
		// either; let it through without a cookie, same as the upgrade
		// case above.
		return false
	}
	token := r.URL.Query().Get(h.cfg.AuthzCookieQueryParam)
	if token == "" {
		return false
	}
	if !validCookieValue(token) {
		// net/http silently drops any byte outside the RFC 6265
		// cookie-octet grammar when writing Set-Cookie (see
		// sanitizeCookieValue in the standard library) rather than
		// erroring — bootstrapping anyway would plant a cookie that no
		// longer equals the credential it was minted from, failing
		// every later request's authorization with nothing pointing at
		// why. This request's own credential already passed Authorize
		// via the query parameter, so let it through as usual; only the
		// cookie session is skipped.
		return false
	}

	http.SetCookie(w, &http.Cookie{
		Name:  h.cfg.AuthzCookieName,
		Value: token,
		// Scoped to exactly the sandbox this token authorized: the
		// path-routing prefix plus (namespace, id, port). A cookie
		// minted for one sandbox's path is never attached by the
		// browser to a request under a different sandbox's path, so
		// the browser itself enforces per-sandbox isolation before the
		// router's own per-request check ever runs.
		Path:     bootstrapCookiePath(h.cfg.PathRoutingPrefix, target),
		HttpOnly: true,
		Secure:   !h.cfg.AuthzCookieInsecure,
		SameSite: sameSiteFor(h.cfg.AuthzCookieSameSite),
		// No Max-Age/Expires: a session cookie. The token still carries
		// its own expiry; once that lapses, the browser needs a fresh
		// bootstrap URL rather than a silent renewal this router has no
		// safe way to grant on its own.
	})

	redirectURL := *r.URL
	// Reuse stripQueryParam rather than Query()+Encode(): the latter
	// resorts every surviving parameter alphabetically and re-escapes
	// each one, which can change a client's own encoding of a value it
	// never asked to have touched. This is exactly the byte-for-byte
	// preservation stripQueryParam exists for elsewhere in this file.
	redirectURL.RawQuery = stripQueryParam(r.URL.RawQuery, h.cfg.AuthzCookieQueryParam)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
	return true
}

// bootstrapCookiePath builds the cookie Path that scopes a bootstrapped
// credential to exactly one sandbox: <prefix>/<namespace>/<id>/<port>/.
// This is the same shape ParsePathRoute expects on the way in, so the
// browser only ever attaches the cookie to requests already addressed
// to this same (namespace, id, port).
func bootstrapCookiePath(prefix string, target Target) string {
	return prefix + "/" + target.Namespace + "/" + target.ID + "/" + strconv.Itoa(target.Port) + "/"
}

// validCookieValue reports whether s is safe to use as an RFC 6265
// cookie-value verbatim:
//
//	cookie-octet = %x21 / %x23-2B / %x2D-3A / %x3C-5B / %x5D-7E
//
// i.e. printable US-ASCII excluding DQUOTE, comma, semicolon, backslash,
// whitespace, and control characters. A scoped-token credential (the
// only kind this router mints itself) is always base64url, well inside
// this set — the check exists for tokenreview mode, where the "token"
// is an arbitrary caller-supplied bearer credential this package does
// not otherwise constrain.
func validCookieValue(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		b := s[i]
		switch {
		case b == 0x21, b >= 0x23 && b <= 0x2B, b >= 0x2D && b <= 0x3A,
			b >= 0x3C && b <= 0x5B, b >= 0x5D && b <= 0x7E:
		default:
			return false
		}
	}
	return true
}

// sameSiteFor maps the operator-facing enum to the net/http constant.
func sameSiteFor(s config.CookieSameSite) http.SameSite {
	switch s {
	case config.CookieSameSiteStrict:
		return http.SameSiteStrictMode
	case config.CookieSameSiteNone:
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

// stripQueryParam removes every "key=value" pair whose key decodes to
// param from a raw (still-encoded) query string, leaving the encoding
// and relative order of every other pair untouched. Returns rawQuery
// unchanged — the same string, not a rebuilt equivalent — whenever
// param is empty or not present, so a deployment that hasn't set
// --authz-cookie-query-param sees byte-identical output to before this
// function existed.
func stripQueryParam(rawQuery, param string) string {
	if param == "" || rawQuery == "" {
		return rawQuery
	}
	pairs := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(pairs))
	changed := false
	for _, p := range pairs {
		key, _, _ := strings.Cut(p, "=")
		if decoded, err := url.QueryUnescape(key); err == nil && decoded == param {
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	if !changed {
		return rawQuery
	}
	return strings.Join(kept, "&")
}

// stripCookieFromHeader removes exactly the cookie named name from a
// Cookie header value, leaving any other cookies the client sent (a
// sandbox's own app, e.g. code-server, sets its own) untouched. Returns
// "" when nothing remains, so the caller can delete the header entirely
// rather than forward an empty one.
func stripCookieFromHeader(header, name string) string {
	if header == "" || name == "" {
		return header
	}
	parts := strings.Split(header, ";")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		key, _, _ := strings.Cut(trimmed, "=")
		if key == name {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}
