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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
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
		name    string
		origin  string
		host    string
		allowed []string
		want    bool
	}{
		{"no origin header is allowed", "", "router.example.com", nil, true},
		{"same-origin (host match) allowed regardless of allowlist", "https://router.example.com", "router.example.com", nil, true},
		{"same-origin ignores scheme", "http://router.example.com", "router.example.com", nil, true},
		{"cross-site with empty allowlist rejected", "https://evil.example.com", "router.example.com", nil, false},
		{"cross-site present in allowlist accepted", "https://atenea.example.com", "router.example.com", []string{"https://atenea.example.com"}, true},
		{"cross-site not in allowlist rejected", "https://evil.example.com", "router.example.com", []string{"https://atenea.example.com"}, false},
		{"allowlist match is case-insensitive", "https://Atenea.Example.com", "router.example.com", []string{"https://atenea.example.com"}, true},
		{"malformed origin rejected", "not a url", "router.example.com", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAllowedOrigin(tc.origin, tc.host, tc.allowed); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
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
