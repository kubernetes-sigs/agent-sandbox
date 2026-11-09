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

// Package bodyrewrite is the bounded response-body rewrite pipeline the
// sandbox-router applies to path-routed responses. A Profile names the
// request paths and response media types it applies to, a size limit, and an
// ordered list of Rules. The framework decides whether a response may be
// buffered at all and fixes up the framing after a rewrite; the Rules only
// transform bytes.
//
// A body is buffered only when all of these hold:
//
//   - the request is not a protocol upgrade or a HEAD, and the status is not
//     1xx, 204, 206 or 304;
//   - the response has no Content-Encoding (other than identity), no
//     Content-Range, and no Cache-Control: no-transform;
//   - its media type and the request path match a configured profile;
//   - its Content-Length is known and within the profile's limit, which is
//     itself capped by the router-wide limit.
//
// Everything else streams through unchanged. When no Rule changes the body,
// the response, headers included, is left exactly as it arrived.
//
// No profiles are built in yet. A built-in profile is added by a file in
// this package that puts its Profile into builtins from an init function.
package bodyrewrite

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Context is what a Rule knows about the response it is rewriting.
type Context struct {
	// Path is the request path as the router matched it: the upstream path
	// with the routing prefix stripped, decoded. Profiles match on this in
	// both routing modes, the same path authorization sees.
	Path string
	// Prefix is the sandbox's routing base,
	// <prefix>/<namespace>/<id>/<port>, with no trailing slash.
	Prefix string
	// PreservePrefix reports whether the router forwarded the full prefixed
	// path (--path-routing-preserve-prefix), in which case the upstream
	// already knows its prefix.
	PreservePrefix bool
	// Upgrade reports a protocol-upgrade request. Such responses are never
	// rewritten; it is part of Context so the caller passes one value.
	Upgrade bool
	// MediaType is the response's lowercased media type without
	// parameters. Set by the framework; ignored on input.
	MediaType string
}

// Rule transforms a response body. It returns the new body and whether it
// changed anything. A Rule must not keep or modify body after returning, and
// must be safe for concurrent use: one Rule value serves every request.
type Rule interface {
	Rewrite(body []byte, c Context) (out []byte, changed bool, err error)
}

// RuleFunc adapts a function to Rule.
type RuleFunc func(body []byte, c Context) ([]byte, bool, error)

// Rewrite implements Rule.
func (f RuleFunc) Rewrite(body []byte, c Context) ([]byte, bool, error) { return f(body, c) }

// Profile selects the responses a set of Rules applies to.
type Profile struct {
	// Name identifies the profile in configuration and logs.
	Name string
	// Paths are glob patterns matched against Context.Path. A pattern
	// starts with "/" or "**". Within a segment, path.Match syntax
	// applies; a "**" segment matches zero or more whole segments, so
	// "/lab/**" matches /lab, /lab/ and /lab/tree/x, and "**/*.html"
	// matches any .html file.
	Paths []string
	// ContentTypes are the media types the profile applies to, such as
	// "text/html", without parameters. Matching ignores case and the
	// response's parameters (charset and so on).
	ContentTypes []string
	// MaxBodyBytes caps the bodies this profile buffers. Zero means the
	// router-wide limit; a larger value is still capped by it.
	MaxBodyBytes int64
	// Rules run in order, each on the previous one's output.
	Rules []Rule
}

// builtins holds the profiles that can be selected by name. Built-in
// profiles add themselves here from an init function in their own file.
var builtins = map[string]Profile{}

// Builtin returns the built-in profile called name.
func Builtin(name string) (Profile, bool) {
	p, ok := builtins[name]
	return p, ok
}

// BuiltinNames returns the names of the built-in profiles, sorted.
func BuiltinNames() []string {
	names := make([]string, 0, len(builtins))
	for n := range builtins {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Resolve looks up the named built-in profiles, in order.
func Resolve(names []string) ([]Profile, error) {
	out := make([]Profile, 0, len(names))
	for _, n := range names {
		p, ok := Builtin(n)
		if !ok {
			known := "none"
			if all := BuiltinNames(); len(all) > 0 {
				known = strings.Join(all, ", ")
			}
			return nil, fmt.Errorf("unknown body rewrite profile %q (built-in profiles: %s)", n, known)
		}
		out = append(out, p)
	}
	return out, nil
}

// RuleError reports a Rule failure. The response it was returned for is
// still intact: its original body is in place and its headers are unchanged,
// so the caller can log the error and serve the response.
type RuleError struct {
	Profile string
	Err     error
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("body rewrite profile %q: %v", e.Profile, e.Err)
}

func (e *RuleError) Unwrap() error { return e.Err }

// Rewriter applies a fixed list of profiles. A nil *Rewriter is valid and
// rewrites nothing.
type Rewriter struct {
	profiles []compiledProfile
}

type compiledProfile struct {
	name         string
	paths        []pattern
	contentTypes []string
	maxBytes     int64
	rules        []Rule
}

// NewRewriter validates and compiles profiles. maxBodyBytes is the
// router-wide cap on any buffered body. It returns nil, and no error, when
// profiles is empty: body rewriting is then disabled.
func NewRewriter(profiles []Profile, maxBodyBytes int64) (*Rewriter, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	if maxBodyBytes <= 0 {
		return nil, fmt.Errorf("body rewrite max bytes must be positive, got %d", maxBodyBytes)
	}
	rw := &Rewriter{}
	seen := map[string]bool{}
	for _, p := range profiles {
		if p.Name == "" {
			return nil, errors.New("body rewrite profile has no name")
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("body rewrite profile %q listed twice", p.Name)
		}
		seen[p.Name] = true
		cp := compiledProfile{name: p.Name, maxBytes: maxBodyBytes}
		if p.MaxBodyBytes < 0 {
			return nil, fmt.Errorf("body rewrite profile %q: max body bytes must not be negative", p.Name)
		}
		if p.MaxBodyBytes > 0 && p.MaxBodyBytes < maxBodyBytes {
			cp.maxBytes = p.MaxBodyBytes
		}
		if len(p.Paths) == 0 {
			return nil, fmt.Errorf("body rewrite profile %q has no paths", p.Name)
		}
		for _, s := range p.Paths {
			m, err := compilePattern(s)
			if err != nil {
				return nil, fmt.Errorf("body rewrite profile %q: %w", p.Name, err)
			}
			cp.paths = append(cp.paths, m)
		}
		if len(p.ContentTypes) == 0 {
			return nil, fmt.Errorf("body rewrite profile %q has no content types", p.Name)
		}
		for _, ct := range p.ContentTypes {
			mt, params, err := mime.ParseMediaType(ct)
			if err != nil || len(params) > 0 || !strings.Contains(mt, "/") {
				return nil, fmt.Errorf("body rewrite profile %q: content type %q must be a bare media type such as text/html", p.Name, ct)
			}
			cp.contentTypes = append(cp.contentTypes, mt)
		}
		if len(p.Rules) == 0 {
			return nil, fmt.Errorf("body rewrite profile %q has no rules", p.Name)
		}
		for _, r := range p.Rules {
			if r == nil {
				return nil, fmt.Errorf("body rewrite profile %q has a nil rule", p.Name)
			}
		}
		cp.rules = slices.Clone(p.Rules)
		rw.profiles = append(rw.profiles, cp)
	}
	return rw, nil
}

// Apply rewrites resp's body with the first profile that matches c.Path and
// the response's media type, if the response may be buffered at all (see the
// package doc). It changes resp only when a Rule changed the body.
//
// A *RuleError leaves resp intact and servable. Any other error means the
// upstream body could not be read in full; resp must not be served.
func (rw *Rewriter) Apply(resp *http.Response, c Context) error {
	if rw == nil || c.Upgrade || !bufferable(resp) {
		return nil
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil
	}
	c.MediaType = mt
	p := rw.match(c.Path, mt)
	if p == nil || resp.ContentLength < 0 || resp.ContentLength > p.maxBytes {
		return nil
	}

	orig := make([]byte, resp.ContentLength)
	_, err = io.ReadFull(resp.Body, orig)
	if err != nil {
		return fmt.Errorf("body rewrite profile %q: reading upstream body: %w", p.name, err)
	}
	_ = resp.Body.Close()

	body, changed := orig, false
	for _, r := range p.rules {
		out, ch, err := r.Rewrite(body, c)
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(orig))
			return &RuleError{Profile: p.name, Err: err}
		}
		if ch {
			body, changed = out, true
		}
	}
	if !changed || bytes.Equal(body, orig) {
		resp.Body = io.NopCloser(bytes.NewReader(orig))
		return nil
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.TransferEncoding = nil
	resp.Header.Del("Transfer-Encoding")
	// These describe the bytes the upstream sent, not the rewritten ones.
	// Accept-Ranges goes too: a later Range request is answered by the
	// upstream with offsets into the original body.
	for _, h := range []string{"ETag", "Content-MD5", "Digest", "Repr-Digest", "Content-Digest", "Accept-Ranges"} {
		resp.Header.Del(h)
	}
	return nil
}

func (rw *Rewriter) match(p, mediaType string) *compiledProfile {
	if p == "" {
		p = "/"
	}
	for i := range rw.profiles {
		cp := &rw.profiles[i]
		if !slices.Contains(cp.contentTypes, mediaType) {
			continue
		}
		for _, m := range cp.paths {
			if m.match(p) {
				return cp
			}
		}
	}
	return nil
}

// bufferable reports whether resp is a response whose body the framework may
// read and replace, before looking at profiles.
func bufferable(resp *http.Response) bool {
	switch {
	case resp.StatusCode < 200,
		resp.StatusCode == http.StatusNoContent,
		resp.StatusCode == http.StatusPartialContent,
		resp.StatusCode == http.StatusNotModified:
		return false
	case resp.Request != nil && resp.Request.Method == http.MethodHead:
		return false
	case resp.Header.Get("Content-Range") != "":
		return false
	}
	// Encoded bodies are never decompressed, so a rule could not read them.
	for _, v := range resp.Header.Values("Content-Encoding") {
		for enc := range strings.SplitSeq(v, ",") {
			if enc = strings.TrimSpace(enc); enc != "" && !strings.EqualFold(enc, "identity") {
				return false
			}
		}
	}
	// RFC 9111 §5.2.2.6: an intermediary must not transform such a body.
	for _, v := range resp.Header.Values("Cache-Control") {
		for d := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(d), "no-transform") {
				return false
			}
		}
	}
	return true
}

// pattern is a compiled glob over "/"-separated path segments.
type pattern struct {
	segs []string
}

func compilePattern(s string) (pattern, error) {
	if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "**") {
		return pattern{}, fmt.Errorf("path pattern %q must start with \"/\" or \"**\"", s)
	}
	segs := strings.Split(strings.TrimPrefix(s, "/"), "/")
	for _, seg := range segs {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return pattern{}, fmt.Errorf("path pattern %q: \"**\" must be a whole segment", s)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return pattern{}, fmt.Errorf("path pattern %q: %w", s, err)
		}
	}
	return pattern{segs: segs}, nil
}

func (m pattern) match(p string) bool {
	return matchSegs(m.segs, strings.Split(strings.TrimPrefix(p, "/"), "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegs(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
