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
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// bootstrapServer builds a router with the browser-session cookie
// feature enabled against a scoped-token authorizer, and returns it
// alongside the secret and a valid token for (namespace, id, port).
func bootstrapServer(t *testing.T, namespace, id string, port int) (*httptest.Server, []byte, string) {
	t.Helper()
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := authz.MintScopedToken(secret, namespace, id, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	a, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		Secret:         secret,
		TokenLocations: authz.TokenLocations{QueryParam: "token", CookieName: "sid"},
	})
	if err != nil {
		t.Fatalf("new authorizer: %v", err)
	}
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	cfg.PathRoutingPrefix = "/router"
	cfg.AuthzMode = config.AuthzScopedToken
	cfg.AuthzCookieName = "sid"
	cfg.AuthzCookieQueryParam = "token"
	// AuthzCookieInsecure deliberately left false (the default): Go's
	// http.Cookie always writes the Secure attribute into the Set-Cookie
	// header text regardless of the connection's own scheme — only a
	// real browser enforces it client-side — so httptest being plain
	// HTTP doesn't require relaxing it here, and leaving it at the
	// default lets TestBootstrapCookie_SetsSessionCookieAndRedirects
	// assert on the real default.

	router := httptest.NewServer(NewHandler(Options{
		Config:     &cfg,
		Authorizer: a,
		Logger:     logr.Discard(),
	}))
	return router, secret, tok
}

// noRedirectClient never follows redirects, so the test can inspect the
// 302 response itself instead of whatever it points to.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func TestBootstrapCookie_SetsSessionCookieAndRedirects(t *testing.T) {
	router, _, tok := bootstrapServer(t, "team", "box-a", 8080)
	defer router.Close()

	resp, err := noRedirectClient().Get(router.URL + "/router/team/box-a/8080/workbench?foo=bar&token=" + tok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want %d", resp.StatusCode, http.StatusFound)
	}

	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one Set-Cookie, got %d: %+v", len(cookies), cookies)
	}
	c := cookies[0]
	if c.Name != "sid" || c.Value != tok {
		t.Fatalf("cookie: got %s=%s, want sid=%s", c.Name, c.Value, tok)
	}
	if c.Path != "/router/team/box-a/8080/" {
		t.Fatalf("cookie Path: got %q, want %q", c.Path, "/router/team/box-a/8080/")
	}
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Fatal("cookie must be Secure by default")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite: got %v want Lax", c.SameSite)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("expected a session cookie (no Max-Age/Expires), got MaxAge=%d Expires=%v", c.MaxAge, c.Expires)
	}

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control: got %q want %q", got, "no-store")
	}

	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if loc.Query().Get("token") != "" {
		t.Fatalf("redirect target must not carry the bootstrap token, got %q", loc.String())
	}
	if loc.Query().Get("foo") != "bar" {
		t.Fatalf("redirect target must preserve other query params, got %q", loc.String())
	}
	if loc.Path != "/router/team/box-a/8080/workbench" {
		t.Fatalf("redirect target path: got %q", loc.Path)
	}
}

// TestBootstrapCookie_RedirectPreservesQueryEncodingAndOrder guards
// against rebuilding the redirect query string via url.Values.Encode(),
// which sorts keys alphabetically and re-escapes every value — losing
// the client's original ordering and encoding of every OTHER param,
// not just the one being removed.
func TestBootstrapCookie_RedirectPreservesQueryEncodingAndOrder(t *testing.T) {
	router, _, tok := bootstrapServer(t, "team", "box-a", 8080)
	defer router.Close()

	// "z" before "token" before "a": alphabetical re-sorting (what
	// url.Values.Encode() would do) would reorder this to "a=2&z=1".
	// "space+tab" is percent-encoded unusually (lowercase hex) on
	// purpose — Encode() would normalize it to Go's own (uppercase hex)
	// escaping.
	resp, err := noRedirectClient().Get(router.URL + "/router/team/box-a/8080/?z=1&token=" + tok + "&a=space%2btab")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want %d", resp.StatusCode, http.StatusFound)
	}

	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	const want = "z=1&a=space%2btab"
	if loc.RawQuery != want {
		t.Fatalf("redirect RawQuery: got %q want %q (order/encoding must survive byte-for-byte)", loc.RawQuery, want)
	}
}

// TestBootstrapCookie_HeaderRoutedRequestNotBootstrapped is the
// regression test for a real bug caught in review: nothing gated the
// bootstrap to a request that actually matched --path-routing-prefix.
// A header-routed GET carrying the bootstrap query parameter (nothing
// stops a header-based caller from also setting it) would get
// redirected and have its only credential stripped, with a cookie
// whose Path could never match a header-routed URL — leaving the
// retried request unauthenticated. It must instead be authorized and
// proxied normally, exactly as if the cookie feature were off.
//
// Uses recordingAuthz (always-allow) rather than ScopedTokenAuthorizer
// so the assertion is purely about the bootstrap's own path-routed
// gate — ScopedTokenAuthorizer separately rejects X-Sandbox-Pod-IP
// (needed here to reach a local httptest backend by header routing at
// all), which would otherwise deny the request for an unrelated reason
// and produce a false pass.
func TestBootstrapCookie_HeaderRoutedRequestNotBootstrapped(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	a := &recordingAuthz{err: nil}
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	cfg.PathRoutingPrefix = "/router" // configured, but this request's path won't match it
	cfg.AuthzCookieName = "sid"
	cfg.AuthzCookieQueryParam = "token"
	router := httptest.NewServer(NewHandler(Options{Config: &cfg, Authorizer: a, Logger: logr.Discard()}))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/x?token=some-credential", nil)
	req.Header.Set(HeaderSandboxID, "box-a")
	req.Header.Set(HeaderSandboxNamespace, "team")
	req.Header.Set(HeaderSandboxPodIP, bu.Hostname())
	req.Header.Set(HeaderSandboxPort, bu.Port())

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d want %d (header-routed request must be authorized and proxied, not bootstrapped)", resp.StatusCode, http.StatusNoContent)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatalf("expected no Set-Cookie for a header-routed request, got %+v", resp.Cookies())
	}
}

// TestCookieStripping_MultipleCookieHeaderLines is the regression test
// for a real gap caught in review: pr.Out.Header.Get("Cookie") only
// ever reads the FIRST "Cookie" header line. A client that sends the
// session cookie on a header line other than the first must still have
// it stripped before the request reaches the sandbox, and every other
// cookie the client sent — regardless of which line it was on — must
// still arrive intact.
func TestCookieStripping_MultipleCookieHeaderLines(t *testing.T) {
	var (
		mu        sync.Mutex
		gotCookie string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCookie = strings.Join(r.Header.Values("Cookie"), "; ")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := authz.MintScopedToken(secret, "team", "box-a", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	a, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		Secret:         secret,
		TokenLocations: authz.TokenLocations{QueryParam: "token", CookieName: "sid"},
	})
	if err != nil {
		t.Fatalf("new authorizer: %v", err)
	}
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	cfg.PathRoutingPrefix = "/router"
	cfg.AuthzMode = config.AuthzScopedToken
	cfg.AuthzCookieName = "sid"
	cfg.AuthzCookieQueryParam = "token"
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"multi-cookie-uid": {PodIP: bu.Hostname(), SandboxName: "box-a", Namespace: "team"},
	}}
	router := httptest.NewServer(NewHandler(Options{Config: &cfg, Cache: lookup, Authorizer: a, Logger: logr.Discard()}))
	defer router.Close()

	req, _ := http.NewRequest("GET", router.URL+"/router/team/box-a/"+bu.Port()+"/", nil)
	// Two distinct "Cookie" header LINES, not one combined "; "-joined
	// value — the credential is on the second line, deliberately not
	// the one Header.Get would see.
	req.Header["Cookie"] = []string{"lang=en", "sid=" + tok}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d want %d", resp.StatusCode, http.StatusNoContent)
	}

	mu.Lock()
	got := gotCookie
	mu.Unlock()
	if strings.Contains(got, "sid=") {
		t.Fatalf("backend saw the session cookie: %q (must be stripped regardless of which Cookie header line it arrived on)", got)
	}
	if !strings.Contains(got, "lang=en") {
		t.Fatalf("backend should still see the client's other cookie, got %q", got)
	}
}

func TestBootstrapCookie_InvalidTokenSetsNoCookie(t *testing.T) {
	router, _, _ := bootstrapServer(t, "team", "box-a", 8080)
	defer router.Close()

	resp, err := noRedirectClient().Get(router.URL + "/router/team/box-a/8080/workbench?token=garbage")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatalf("expected no Set-Cookie for an invalid token, got %+v", resp.Cookies())
	}
}

func TestBootstrapCookie_TokenScopedToOtherSandboxSetsNoCookie(t *testing.T) {
	router, secret, _ := bootstrapServer(t, "team", "box-a", 8080)
	defer router.Close()

	otherTok, err := authz.MintScopedToken(secret, "team", "box-b", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resp, err := noRedirectClient().Get(router.URL + "/router/team/box-a/8080/workbench?token=" + otherTok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatalf("expected no Set-Cookie when the token is scoped to a different sandbox, got %+v", resp.Cookies())
	}
}

func TestBootstrapCookie_DifferentSandboxesGetDifferentCookiePaths(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	a, _ := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		Secret:         secret,
		TokenLocations: authz.TokenLocations{QueryParam: "token", CookieName: "sid"},
	})
	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 2 * time.Second
	cfg.PathRoutingPrefix = "/router"
	cfg.AuthzMode = config.AuthzScopedToken
	cfg.AuthzCookieName = "sid"
	cfg.AuthzCookieQueryParam = "token"
	router := httptest.NewServer(NewHandler(Options{Config: &cfg, Authorizer: a, Logger: logr.Discard()}))
	defer router.Close()

	tokA, _ := authz.MintScopedToken(secret, "team", "box-a", time.Minute)
	tokB, _ := authz.MintScopedToken(secret, "team", "box-b", time.Minute)

	respA, err := noRedirectClient().Get(router.URL + "/router/team/box-a/8080/?token=" + tokA)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	respA.Body.Close()
	respB, err := noRedirectClient().Get(router.URL + "/router/team/box-b/9090/?token=" + tokB)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	respB.Body.Close()

	pathA := respA.Cookies()[0].Path
	pathB := respB.Cookies()[0].Path
	if pathA == pathB {
		t.Fatalf("expected distinct cookie paths for distinct sandboxes, both got %q", pathA)
	}
	if pathA != "/router/team/box-a/8080/" || pathB != "/router/team/box-b/9090/" {
		t.Fatalf("unexpected cookie paths: a=%q b=%q", pathA, pathB)
	}
}

func TestBootstrapCookie_SameSiteNoneRequiresSecure(t *testing.T) {
	got := sameSiteFor(config.CookieSameSiteNone)
	if got != http.SameSiteNoneMode {
		t.Fatalf("got %v want SameSiteNoneMode", got)
	}
	got = sameSiteFor(config.CookieSameSiteStrict)
	if got != http.SameSiteStrictMode {
		t.Fatalf("got %v want SameSiteStrictMode", got)
	}
	got = sameSiteFor(config.CookieSameSiteLax)
	if got != http.SameSiteLaxMode {
		t.Fatalf("got %v want SameSiteLaxMode", got)
	}
}

func TestIsAllowedOrigin(t *testing.T) {
	cases := []struct {
		name       string
		origin     string
		selfOrigin string
		allowed    []string
		want       bool
	}{
		{"no origin header is allowed", "", "https://router.example.com", nil, true},
		{"same-origin (scheme+host match) allowed regardless of allowlist", "https://router.example.com", "https://router.example.com", nil, true},
		{
			name:       "scheme mismatch is NOT treated as same-origin",
			origin:     "http://router.example.com",
			selfOrigin: "https://router.example.com",
			allowed:    nil,
			want:       false,
		},
		{
			name:       "scheme mismatch the other direction is also rejected",
			origin:     "https://router.example.com",
			selfOrigin: "http://router.example.com",
			allowed:    nil,
			want:       false,
		},
		{"cross-site with empty allowlist rejected", "https://evil.example.com", "https://router.example.com", nil, false},
		{"cross-site present in allowlist accepted", "https://atenea.example.com", "https://router.example.com", []string{"https://atenea.example.com"}, true},
		{"cross-site not in allowlist rejected", "https://evil.example.com", "https://router.example.com", []string{"https://atenea.example.com"}, false},
		{"allowlist match is case-insensitive", "https://Atenea.Example.com", "https://router.example.com", []string{"https://atenea.example.com"}, true},
		{"malformed origin rejected", "not a url", "https://router.example.com", nil, false},
		{
			name:       "allowlist entry with explicit default https port matches an origin without one",
			origin:     "https://atenea.example.com",
			selfOrigin: "https://router.example.com",
			allowed:    []string{"https://atenea.example.com:443"},
			want:       true,
		},
		{
			name:       "self-origin with explicit default port matches an origin without one",
			origin:     "https://router.example.com",
			selfOrigin: "https://router.example.com:443",
			allowed:    nil,
			want:       true,
		},
		{
			name:       "a non-default port is never stripped and must match exactly",
			origin:     "https://atenea.example.com",
			selfOrigin: "https://router.example.com",
			allowed:    []string{"https://atenea.example.com:8443"},
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAllowedOrigin(tc.origin, tc.selfOrigin, tc.allowed); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRequestOrigin(t *testing.T) {
	plain, _ := http.NewRequest("GET", "http://router.example.com/x", nil)
	plain.Host = "router.example.com"
	if got := requestOrigin(plain); got != "http://router.example.com" {
		t.Fatalf("plain: got %q", got)
	}

	tlsReq, _ := http.NewRequest("GET", "https://router.example.com/x", nil)
	tlsReq.Host = "router.example.com"
	tlsReq.TLS = &tls.ConnectionState{}
	if got := requestOrigin(tlsReq); got != "https://router.example.com" {
		t.Fatalf("tls: got %q", got)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com", "https://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"http://example.com:80", "http://example.com"},
		{"http://example.com:8080", "http://example.com:8080"},
		{"https://example.com:8443", "https://example.com:8443"},
		{"not a url", "not a url"},
	}
	for _, tc := range cases {
		if got := normalizeOrigin(tc.in); got != tc.want {
			t.Fatalf("normalizeOrigin(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripQueryParam(t *testing.T) {
	cases := []struct {
		name  string
		query string
		param string
		want  string
	}{
		{"empty param is a no-op", "a=1&b=2", "", "a=1&b=2"},
		{"empty query is a no-op", "", "token", ""},
		{"param absent leaves query untouched", "a=1&b=2", "token", "a=1&b=2"},
		{"removes the only param", "token=abc", "token", ""},
		{"removes leading param, keeps order of the rest", "token=abc&a=1&b=2", "token", "a=1&b=2"},
		{"removes middle param, keeps order of the rest", "a=1&token=abc&b=2", "token", "a=1&b=2"},
		{"removes trailing param, keeps order of the rest", "a=1&b=2&token=abc", "token", "a=1&b=2"},
		{"matches a percent-encoded key", "a=1&t%6fken=abc", "token", "a=1"},
		{"does not touch an unrelated value that contains the param name", "a=token123", "token", "a=token123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripQueryParam(tc.query, tc.param); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestStripCookieFromHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		cookie string
		want   string
	}{
		{"empty header is a no-op", "", "sid", ""},
		{"empty name is a no-op", "sid=abc", "", "sid=abc"},
		{"removes the only cookie", "sid=abc", "sid", ""},
		{"removes our cookie, keeps others", "sid=abc; theme=dark", "sid", "theme=dark"},
		{"our cookie absent leaves others untouched", "theme=dark; lang=en", "sid", "theme=dark; lang=en"},
		{"removes our cookie from the middle", "theme=dark; sid=abc; lang=en", "sid", "theme=dark; lang=en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCookieFromHeader(tc.header, tc.cookie); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestBootstrapCookiePath(t *testing.T) {
	target := Target{Namespace: "team", ID: "box-a", Port: 8080}
	got := bootstrapCookiePath("/router", target)
	want := "/router/team/box-a/8080/"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
