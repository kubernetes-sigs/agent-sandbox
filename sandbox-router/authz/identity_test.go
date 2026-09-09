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
	"testing"
)

func TestBearerTokenFromRequest(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{"missing header", "", "", false},
		{"basic auth ignored", "Basic dXNlcjpwYXNz", "", false},
		{"happy path", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"case insensitive scheme", "bearer abc", "abc", true},
		{"BEARER scheme", "BEARER xyz", "xyz", true},
		{"trims whitespace around token", "Bearer    spaced.token   ", "spaced.token", true},
		{"empty token rejected", "Bearer ", "", false},
		{"empty token after trim rejected", "Bearer    ", "", false},
		{"shorter than prefix", "Bear", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set(AuthorizationHeader, tc.header)
			}
			tok, ok := BearerTokenFromRequest(req)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v (token=%q)", ok, tc.wantOK, tok)
			}
			if tok != tc.wantToken {
				t.Fatalf("token: got %q want %q", tok, tc.wantToken)
			}
		})
	}
}

func TestBearerTokenFromRequestNilRequest(t *testing.T) {
	if _, ok := BearerTokenFromRequest(nil); ok {
		t.Fatal("nil request should not produce a token")
	}
}

// reqWithLocations builds a request carrying zero or more of: an
// Authorization header, a query parameter, and a cookie — whichever of
// header/query/param is non-empty is set under the given names.
func reqWithLocations(t *testing.T, header, queryParam, queryVal, cookieName, cookieVal string) *http.Request {
	t.Helper()
	u := "/"
	if queryParam != "" {
		u = "/?" + queryParam + "=" + queryVal
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if header != "" {
		req.Header.Set(AuthorizationHeader, "Bearer "+header)
	}
	if cookieName != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieVal})
	}
	return req
}

func TestTokenFromRequest(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		queryVal   string
		cookieVal  string
		loc        TokenLocations
		wantToken  string
		wantSource TokenSource
		wantOK     bool
	}{
		{
			name:      "zero value TokenLocations behaves like BearerTokenFromRequest: header found",
			header:    "h-tok",
			loc:       TokenLocations{},
			wantToken: "h-tok", wantSource: TokenSourceHeader, wantOK: true,
		},
		{
			name: "zero value TokenLocations: no header, nothing else checked even if present in URL",
			loc:  TokenLocations{},
			// queryVal/cookieVal deliberately left unset: with disabled
			// locations there is nothing to find regardless.
			wantOK: false,
		},
		{
			name:      "query enabled, no header: query wins",
			queryVal:  "q-tok",
			loc:       TokenLocations{QueryParam: "token"},
			wantToken: "q-tok", wantSource: TokenSourceQuery, wantOK: true,
		},
		{
			name:      "cookie enabled, no header, no query: cookie wins",
			cookieVal: "c-tok",
			loc:       TokenLocations{CookieName: "sid"},
			wantToken: "c-tok", wantSource: TokenSourceCookie, wantOK: true,
		},
		{
			name:      "header beats query and cookie",
			header:    "h-tok",
			queryVal:  "q-tok",
			cookieVal: "c-tok",
			loc:       TokenLocations{QueryParam: "token", CookieName: "sid"},
			wantToken: "h-tok", wantSource: TokenSourceHeader, wantOK: true,
		},
		{
			name:      "query beats cookie when both present and no header",
			queryVal:  "q-tok",
			cookieVal: "c-tok",
			loc:       TokenLocations{QueryParam: "token", CookieName: "sid"},
			wantToken: "q-tok", wantSource: TokenSourceQuery, wantOK: true,
		},
		{
			name:      "query param configured but absent falls through to cookie",
			cookieVal: "c-tok",
			loc:       TokenLocations{QueryParam: "token", CookieName: "sid"},
			wantToken: "c-tok", wantSource: TokenSourceCookie, wantOK: true,
		},
		{
			name:   "nothing anywhere",
			loc:    TokenLocations{QueryParam: "token", CookieName: "sid"},
			wantOK: false,
		},
		{
			name:      "empty cookie value treated as absent",
			cookieVal: "",
			loc:       TokenLocations{CookieName: "sid"},
			wantOK:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := reqWithLocations(t, tc.header, tc.loc.QueryParam, tc.queryVal, tc.loc.CookieName, tc.cookieVal)
			tok, src, ok := TokenFromRequest(req, tc.loc)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v (token=%q source=%q)", ok, tc.wantOK, tok, src)
			}
			if !tc.wantOK {
				return
			}
			if tok != tc.wantToken {
				t.Fatalf("token: got %q want %q", tok, tc.wantToken)
			}
			if src != tc.wantSource {
				t.Fatalf("source: got %q want %q", src, tc.wantSource)
			}
		})
	}
}

func TestTokenFromRequestNilRequest(t *testing.T) {
	if _, _, ok := TokenFromRequest(nil, TokenLocations{QueryParam: "token", CookieName: "sid"}); ok {
		t.Fatal("nil request should not produce a token")
	}
}
