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

package bodyrewrite

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// replaceRule is the fake rule the framework is tested with: it replaces
// every "OLD" with the request prefix, and reports no change when the body
// has no "OLD" in it.
type replaceRule struct{ calls int }

func (r *replaceRule) Rewrite(body []byte, c Context) ([]byte, bool, error) {
	r.calls++
	if !bytes.Contains(body, []byte("OLD")) {
		return body, false, nil
	}
	return bytes.ReplaceAll(body, []byte("OLD"), []byte(c.Prefix)), true, nil
}

type failingRule struct{}

func (failingRule) Rewrite([]byte, Context) ([]byte, bool, error) {
	return nil, false, errors.New("boom")
}

// identityRule claims a change but returns the same bytes.
type identityRule struct{}

func (identityRule) Rewrite(body []byte, _ Context) ([]byte, bool, error) {
	return append([]byte(nil), body...), true, nil
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/lab/**", "/lab", true},
		{"/lab/**", "/lab/", true},
		{"/lab/**", "/lab/tree/a/b", true},
		{"/lab/**", "/labs", false},
		{"/lab/**", "/api/lab", false},
		{"/lab", "/lab", true},
		{"/lab", "/lab/", false},
		{"/", "/", true},
		{"/", "/x", false},
		{"/api/*", "/api/kernelspecs", true},
		{"/api/*", "/api/kernelspecs/python3", false},
		{"/api/*", "/api", false},
		{"/api/kernel*", "/api/kernelspecs", true},
		{"**/*.html", "/index.html", true},
		{"**/*.html", "/a/b/c.html", true},
		{"**/*.html", "/a/b/c.htm", false},
		{"**", "/anything/at/all", true},
		{"/a/**/z", "/a/z", true},
		{"/a/**/z", "/a/b/c/z", true},
		{"/a/**/z", "/a/b/c/y", false},
	}
	for _, tc := range cases {
		m, err := compilePattern(tc.pattern)
		if err != nil {
			t.Fatalf("compilePattern(%q): %v", tc.pattern, err)
		}
		if got := m.match(tc.path); got != tc.want {
			t.Errorf("pattern %q path %q: got %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestCompilePatternRejectsInvalid(t *testing.T) {
	for _, p := range []string{"", "lab/**", "/lab/[a", "/a**b"} {
		if _, err := compilePattern(p); err == nil {
			t.Errorf("compilePattern(%q) = nil error, want error", p)
		}
	}
}

func TestNewRewriterValidatesProfiles(t *testing.T) {
	rule := &replaceRule{}
	ok := Profile{Name: "fake", Paths: []string{"/**"}, ContentTypes: []string{"text/html"}, Rules: []Rule{rule}}
	cases := []struct {
		name string
		mut  func(p *Profile)
		max  int64
	}{
		{"empty name", func(p *Profile) { p.Name = "" }, 1024},
		{"no paths", func(p *Profile) { p.Paths = nil }, 1024},
		{"bad path", func(p *Profile) { p.Paths = []string{"lab"} }, 1024},
		{"no content types", func(p *Profile) { p.ContentTypes = nil }, 1024},
		{"bad content type", func(p *Profile) { p.ContentTypes = []string{"text/html; charset=utf-8"} }, 1024},
		{"no rules", func(p *Profile) { p.Rules = nil }, 1024},
		{"nil rule", func(p *Profile) { p.Rules = []Rule{nil} }, 1024},
		{"negative profile max", func(p *Profile) { p.MaxBodyBytes = -1 }, 1024},
		{"zero router max", func(*Profile) {}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.mut(&p)
			if _, err := NewRewriter([]Profile{p}, tc.max); err == nil {
				t.Fatal("NewRewriter = nil error, want error")
			}
		})
	}
	if _, err := NewRewriter([]Profile{ok, ok}, 1024); err == nil {
		t.Fatal("duplicate profile names accepted")
	}
	if _, err := NewRewriter([]Profile{ok}, 1024); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

func TestNewRewriterNoProfilesIsDisabled(t *testing.T) {
	rw, err := NewRewriter(nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if rw != nil {
		t.Fatal("NewRewriter with no profiles should return nil (disabled)")
	}
	// A nil Rewriter is a valid no-op so callers need no extra branch.
	resp := newResp("text/html", "OLD")
	if err := rw.Apply(resp, Context{Path: "/", Prefix: "/p"}); err != nil {
		t.Fatal(err)
	}
	assertBody(t, resp, "OLD")
}

func newResp(contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       &http.Request{Method: http.MethodGet},
	}
}

func assertBody(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func fakeRewriter(t *testing.T, rules ...Rule) *Rewriter {
	t.Helper()
	if len(rules) == 0 {
		rules = []Rule{&replaceRule{}}
	}
	rw, err := NewRewriter([]Profile{{
		Name:         "fake",
		Paths:        []string{"/lab/**"},
		ContentTypes: []string{"text/html", "application/json"},
		MaxBodyBytes: 64,
		Rules:        rules,
	}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return rw
}

var ctx = Context{Path: "/lab/tree", Prefix: "/router/ns/box/8888"}

func TestApplyRewritesAndFixesFraming(t *testing.T) {
	rw := fakeRewriter(t)
	resp := newResp("text/html; charset=utf-8", "<a href=\"OLD/x\">")
	resp.Header.Set("ETag", `"abc"`)
	resp.Header.Set("Accept-Ranges", "bytes")
	resp.Header.Set("Content-MD5", "deadbeef")
	resp.Header.Set("Digest", "sha-256=xyz")
	resp.Header.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	resp.TransferEncoding = []string{"chunked"}
	resp.Header.Set("Transfer-Encoding", "chunked")

	if err := rw.Apply(resp, ctx); err != nil {
		t.Fatal(err)
	}
	want := "<a href=\"/router/ns/box/8888/x\">"
	if got := resp.ContentLength; got != int64(len(want)) {
		t.Fatalf("ContentLength = %d, want %d", got, len(want))
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(want)) {
		t.Fatalf("Content-Length header = %q, want %d", got, len(want))
	}
	if resp.TransferEncoding != nil || resp.Header.Get("Transfer-Encoding") != "" {
		t.Fatalf("Transfer-Encoding not cleared: %v / %q", resp.TransferEncoding, resp.Header.Get("Transfer-Encoding"))
	}
	for _, h := range []string{"ETag", "Accept-Ranges", "Content-MD5", "Digest"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("%s = %q, want removed after rewrite", h, v)
		}
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified should be kept")
	}
	assertBody(t, resp, want)
}

func TestApplyNoRuleChangeLeavesResponseAlone(t *testing.T) {
	for name, rule := range map[string]Rule{
		"rule reports no change":           &replaceRule{},
		"rule claims change, same bytes":   identityRule{},
		"rule fails, original body served": failingRule{},
	} {
		t.Run(name, func(t *testing.T) {
			rw := fakeRewriter(t, rule)
			body := "nothing to see"
			resp := newResp("text/html", body)
			resp.Header.Set("ETag", `"abc"`)
			err := rw.Apply(resp, ctx)
			if _, failing := rule.(failingRule); failing {
				if err == nil {
					t.Fatal("want the rule error reported")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if resp.Header.Get("ETag") != `"abc"` {
				t.Fatal("ETag dropped although body unchanged")
			}
			if resp.ContentLength != int64(len(body)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length changed: %d / %q", resp.ContentLength, resp.Header.Get("Content-Length"))
			}
			assertBody(t, resp, body)
		})
	}
}

func TestApplyRulesRunInOrder(t *testing.T) {
	var order []string
	mk := func(name, from, to string) Rule {
		return RuleFunc(func(b []byte, _ Context) ([]byte, bool, error) {
			order = append(order, name)
			if !bytes.Contains(b, []byte(from)) {
				return b, false, nil
			}
			return bytes.ReplaceAll(b, []byte(from), []byte(to)), true, nil
		})
	}
	rw := fakeRewriter(t, mk("first", "a", "b"), mk("second", "b", "c"))
	resp := newResp("text/html", "a")
	if err := rw.Apply(resp, ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("order = %v", order)
	}
	assertBody(t, resp, "c")
}

// TestApplySkips covers every case the framework must stream through
// untouched. The rule would change the body if it ran, so an unchanged body
// together with zero rule calls proves the skip.
func TestApplySkips(t *testing.T) {
	const body = "OLD"
	cases := []struct {
		name string
		mut  func(resp *http.Response, c *Context)
	}{
		{"path does not match", func(_ *http.Response, c *Context) { c.Path = "/api/x" }},
		{"content type does not match", func(r *http.Response, _ *Context) { r.Header.Set("Content-Type", "image/png") }},
		{"no content type", func(r *http.Response, _ *Context) { r.Header.Del("Content-Type") }},
		{"malformed content type", func(r *http.Response, _ *Context) { r.Header.Set("Content-Type", "text/html; =") }},
		{"unknown length (chunked)", func(r *http.Response, _ *Context) {
			r.ContentLength = -1
			r.Header.Del("Content-Length")
			r.TransferEncoding = []string{"chunked"}
		}},
		{"gzip encoded", func(r *http.Response, _ *Context) { r.Header.Set("Content-Encoding", "gzip") }},
		{"br encoded", func(r *http.Response, _ *Context) { r.Header.Set("Content-Encoding", "br") }},
		{"cache-control no-transform", func(r *http.Response, _ *Context) { r.Header.Set("Cache-Control", "private, no-transform") }},
		{"partial content", func(r *http.Response, _ *Context) {
			r.StatusCode = http.StatusPartialContent
			r.Header.Set("Content-Range", "bytes 0-2/10")
		}},
		{"content-range on 200", func(r *http.Response, _ *Context) { r.Header.Set("Content-Range", "bytes 0-2/10") }},
		{"HEAD request", func(r *http.Response, _ *Context) { r.Request.Method = http.MethodHead }},
		{"101 switching protocols", func(r *http.Response, _ *Context) { r.StatusCode = http.StatusSwitchingProtocols }},
		{"103 early hints", func(r *http.Response, _ *Context) { r.StatusCode = http.StatusEarlyHints }},
		{"204 no content", func(r *http.Response, _ *Context) { r.StatusCode = http.StatusNoContent }},
		{"304 not modified", func(r *http.Response, _ *Context) { r.StatusCode = http.StatusNotModified }},
		{"upgrade", func(_ *http.Response, c *Context) { c.Upgrade = true }},
		{"grpc content type", func(r *http.Response, _ *Context) { r.Header.Set("Content-Type", "application/grpc") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := &replaceRule{}
			rw := fakeRewriter(t, rule)
			resp := newResp("text/html", body)
			c := ctx
			tc.mut(resp, &c)
			if err := rw.Apply(resp, c); err != nil {
				t.Fatal(err)
			}
			if rule.calls != 0 {
				t.Fatalf("rule ran %d times, want skipped", rule.calls)
			}
			assertBody(t, resp, body)
		})
	}
}

func TestApplyIdentityEncodingIsRewritten(t *testing.T) {
	rw := fakeRewriter(t)
	resp := newResp("application/json", `{"u":"OLD/a"}`)
	resp.Header.Set("Content-Encoding", "identity")
	if err := rw.Apply(resp, ctx); err != nil {
		t.Fatal(err)
	}
	assertBody(t, resp, `{"u":"/router/ns/box/8888/a"}`)
}

func TestApplySizeLimit(t *testing.T) {
	// The fake profile caps bodies at 64 bytes; the router-wide cap is 1024.
	t.Run("at the profile limit is rewritten", func(t *testing.T) {
		rw := fakeRewriter(t)
		body := "OLD" + strings.Repeat("x", 61)
		resp := newResp("text/html", body)
		if err := rw.Apply(resp, ctx); err != nil {
			t.Fatal(err)
		}
		assertBody(t, resp, ctx.Prefix+strings.Repeat("x", 61))
	})
	t.Run("over the profile limit streams through", func(t *testing.T) {
		rule := &replaceRule{}
		rw := fakeRewriter(t, rule)
		body := "OLD" + strings.Repeat("x", 62)
		resp := newResp("text/html", body)
		if err := rw.Apply(resp, ctx); err != nil {
			t.Fatal(err)
		}
		if rule.calls != 0 {
			t.Fatal("rule ran on an oversized body")
		}
		assertBody(t, resp, body)
	})
	t.Run("router cap wins over a larger profile limit", func(t *testing.T) {
		rule := &replaceRule{}
		rw, err := NewRewriter([]Profile{{
			Name: "big", Paths: []string{"/**"}, ContentTypes: []string{"text/html"},
			MaxBodyBytes: 1 << 20, Rules: []Rule{rule},
		}}, 8)
		if err != nil {
			t.Fatal(err)
		}
		body := "OLD-123456"
		resp := newResp("text/html", body)
		if err := rw.Apply(resp, ctx); err != nil {
			t.Fatal(err)
		}
		if rule.calls != 0 {
			t.Fatal("rule ran on a body over the router cap")
		}
		assertBody(t, resp, body)
	})
	t.Run("profile without a limit uses the router cap", func(t *testing.T) {
		rw, err := NewRewriter([]Profile{{
			Name: "nolimit", Paths: []string{"/**"}, ContentTypes: []string{"text/html"},
			Rules: []Rule{&replaceRule{}},
		}}, 16)
		if err != nil {
			t.Fatal(err)
		}
		resp := newResp("text/html", "OLD")
		if err := rw.Apply(resp, ctx); err != nil {
			t.Fatal(err)
		}
		assertBody(t, resp, ctx.Prefix)
	})
}

// TestApplyShortBodyIsAnError covers an upstream whose body is shorter than
// its declared Content-Length: the response is broken and must not be served
// as if it were whole.
func TestApplyShortBodyIsAnError(t *testing.T) {
	rw := fakeRewriter(t)
	resp := newResp("text/html", "OLD")
	resp.ContentLength = 10
	resp.Header.Set("Content-Length", "10")
	if err := rw.Apply(resp, ctx); err == nil {
		t.Fatal("want an error for a truncated upstream body")
	}
}

func TestApplyFirstMatchingProfileWins(t *testing.T) {
	first, second := &replaceRule{}, &replaceRule{}
	rw, err := NewRewriter([]Profile{
		{Name: "json-only", Paths: []string{"/**"}, ContentTypes: []string{"application/json"}, Rules: []Rule{first}},
		{Name: "html", Paths: []string{"/lab/**"}, ContentTypes: []string{"text/html"}, Rules: []Rule{second}},
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	resp := newResp("TEXT/HTML", "OLD")
	if err := rw.Apply(resp, ctx); err != nil {
		t.Fatal(err)
	}
	if first.calls != 0 || second.calls != 1 {
		t.Fatalf("calls = %d/%d, want 0/1", first.calls, second.calls)
	}
	assertBody(t, resp, ctx.Prefix)
}

func TestApplyPassesContextToRules(t *testing.T) {
	var got Context
	rw := fakeRewriter(t, RuleFunc(func(b []byte, c Context) ([]byte, bool, error) {
		got = c
		return b, false, nil
	}))
	resp := newResp("application/json; charset=utf-8", "{}")
	c := ctx
	c.PreservePrefix = true
	if err := rw.Apply(resp, c); err != nil {
		t.Fatal(err)
	}
	want := c
	want.MediaType = "application/json"
	if got != want {
		t.Fatalf("rule context = %+v, want %+v", got, want)
	}
}

func TestBuiltinRegistry(t *testing.T) {
	if _, ok := Builtin("does-not-exist"); ok {
		t.Fatal("unknown profile reported as built in")
	}
	if _, err := Resolve([]string{"does-not-exist"}); err == nil {
		t.Fatal("Resolve accepted an unknown profile")
	}
	ps, err := Resolve(nil)
	if err != nil || len(ps) != 0 {
		t.Fatalf("Resolve(nil) = %v, %v", ps, err)
	}
}
