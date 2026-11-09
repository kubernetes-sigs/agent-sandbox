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
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/bodyrewrite"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// fakePrefixRule stands in for a real profile: it replaces every "OLD" with
// the sandbox's routing base, and fails on a body containing "FAIL".
var fakePrefixRule = bodyrewrite.RuleFunc(func(b []byte, c bodyrewrite.Context) ([]byte, bool, error) {
	if bytes.Contains(b, []byte("FAIL")) {
		return nil, false, errors.New("fake rule failure")
	}
	if !bytes.Contains(b, []byte("OLD")) {
		return b, false, nil
	}
	return bytes.ReplaceAll(b, []byte("OLD"), []byte(c.Prefix)), true, nil
})

var fakeProfiles = []bodyrewrite.Profile{{
	Name:         "fake",
	Paths:        []string{"/lab/**"},
	ContentTypes: []string{"text/html"},
	MaxBodyBytes: 1024,
	Rules:        []bodyrewrite.Rule{fakePrefixRule},
}}

// startBodyBackend serves a fixed body per path, so each test case picks the
// response shape it needs by path alone.
func startBodyBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	fixed := func(contentType, body string, extra http.Header) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			maps.Copy(w.Header(), extra)
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = io.WriteString(w, body)
		}
	}
	mux.HandleFunc("/lab/page", fixed("text/html; charset=utf-8", `<a href="OLD/x">`, http.Header{
		"Etag":       {`"v1"`},
		"Set-Cookie": {"sid=abc; Path=/"},
	}))
	mux.HandleFunc("/lab/image", fixed("image/png", "OLD", nil))
	mux.HandleFunc("/other", fixed("text/html", "OLD", nil))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, "OLD")
	_ = zw.Close()
	mux.HandleFunc("/lab/gz", fixed("text/html", gz.String(), http.Header{"Content-Encoding": {"gzip"}}))
	mux.HandleFunc("/lab/big", fixed("text/html", "OLD"+string(bytes.Repeat([]byte("x"), 2048)), nil))
	mux.HandleFunc("/lab/fail", fixed("text/html", "OLD FAIL", nil))
	mux.HandleFunc("/lab/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "OLD")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "OLD")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type bodyRouter struct {
	url   string
	base  string
	authz *recordingAuthz
}

func startBodyRouter(t *testing.T, backend *httptest.Server, profiles []bodyrewrite.Profile, authzErr error) bodyRouter {
	t.Helper()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}
	cfg := config.Defaults()
	cfg.PathRoutingPrefix = "/router"
	cfg.AllowLoopbackPodIP = true
	cfg.ProxyTimeout = 5 * time.Second
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"body-uid": {PodIP: bu.Hostname(), SandboxName: "ws-sandbox", Namespace: "test"},
	}}
	a := &recordingAuthz{err: authzErr}
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg, Cache: lookup, Authorizer: a, Logger: logr.Discard(),
		BodyRewriteProfiles: profiles,
	}))
	t.Cleanup(router.Close)
	return bodyRouter{url: router.URL, base: "/router/test/ws-sandbox/" + bu.Port(), authz: a}
}

// rawClient does not ask for or undo compression, so the test sees the body
// bytes the router actually sent.
var rawClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func fetch(t *testing.T, method, u string, h http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	maps.Copy(req.Header, h)
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(b)
}

func TestIntegration_BodyRewriteOnlyForMatchingPathAndContentType(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, fakeProfiles, nil)

	resp, body := fetch(t, http.MethodGet, r.url+r.base+"/lab/page", nil)
	want := `<a href="` + r.base + `/x">`
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if resp.ContentLength != int64(len(want)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("Content-Length = %d / %q, want %d", resp.ContentLength, resp.Header.Get("Content-Length"), len(want))
	}
	if len(resp.TransferEncoding) != 0 {
		t.Fatalf("Transfer-Encoding = %v, want none alongside Content-Length", resp.TransferEncoding)
	}
	if resp.Header.Get("Etag") != "" {
		t.Fatalf("ETag %q kept on a rewritten body", resp.Header.Get("Etag"))
	}
	// The header rewriting from --path-routing-rewrite-responses still runs.
	if c := resp.Cookies(); len(c) != 1 || c[0].Path != r.base+"/" {
		t.Fatalf("cookies = %v, want Path %q", c, r.base+"/")
	}
	// Authorization still sees the stripped path.
	if reqs := r.authz.snapshot(); len(reqs) != 1 || reqs[0].target.Path != "/lab/page" {
		t.Fatalf("authorized %+v, want one call for /lab/page", reqs)
	}

	for _, p := range []string{
		"/lab/image",  // content type not in the profile
		"/other",      // path not in the profile
		"/lab/gz",     // encoded, never decompressed
		"/lab/big",    // over the size limit
		"/lab/stream", // unknown length
	} {
		t.Run(p, func(t *testing.T) {
			// Asking for gzip ourselves keeps the router's transport from
			// adding it and transparently decoding the response.
			h := http.Header{"Accept-Encoding": {"gzip"}}
			direct, wantBody := fetch(t, http.MethodGet, backend.URL+p, h)
			resp, body := fetch(t, http.MethodGet, r.url+r.base+p, h)
			if body != wantBody {
				t.Fatalf("body = %q, want unchanged %q", body, wantBody)
			}
			if resp.ContentLength != direct.ContentLength {
				t.Fatalf("ContentLength = %d, want unchanged %d", resp.ContentLength, direct.ContentLength)
			}
		})
	}
}

func TestIntegration_BodyRewriteRuleErrorServesOriginal(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, fakeProfiles, nil)
	resp, body := fetch(t, http.MethodGet, r.url+r.base+"/lab/fail", nil)
	if resp.StatusCode != http.StatusOK || body != "OLD FAIL" {
		t.Fatalf("got %d %q, want 200 with the original body", resp.StatusCode, body)
	}
}

func TestIntegration_BodyRewriteSkipsHEAD(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, fakeProfiles, nil)
	resp, _ := fetch(t, http.MethodHead, r.url+r.base+"/lab/page", nil)
	if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(`<a href="OLD/x">`)); got != want {
		t.Fatalf("HEAD Content-Length = %q, want upstream's %q", got, want)
	}
	if resp.Header.Get("Etag") == "" {
		t.Fatal("HEAD lost its ETag")
	}
}

func TestIntegration_BodyRewriteDisabledByDefault(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, nil, nil)
	if _, body := fetch(t, http.MethodGet, r.url+r.base+"/lab/page", nil); body != `<a href="OLD/x">` {
		t.Fatalf("body = %q, want unchanged with no profiles configured", body)
	}
}

func TestIntegration_BodyRewriteHeaderRoutedUntouched(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, fakeProfiles, nil)
	if _, body := fetch(t, http.MethodGet, r.url+"/lab/page", headerRoutedTo(t, backend.URL)); body != `<a href="OLD/x">` {
		t.Fatalf("header-routed body = %q, want unchanged", body)
	}
}

func TestIntegration_BodyRewriteNotReachedWhenUnauthorized(t *testing.T) {
	backend := startBodyBackend(t)
	r := startBodyRouter(t, backend, fakeProfiles, authz.ErrForbidden)
	resp, body := fetch(t, http.MethodGet, r.url+r.base+"/lab/page", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if bytes.Contains([]byte(body), []byte("href")) {
		t.Fatalf("denied request leaked the upstream body: %q", body)
	}
}
