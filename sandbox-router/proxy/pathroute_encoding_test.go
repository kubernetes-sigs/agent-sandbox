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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	sandboxdserver "sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// TestPathRoutingPreservesEncodedSlash is the end-to-end regression test
// for the escaping bug caught in review: r.URL.Path is already
// percent-decoded, so naively deriving the upstream remainder from it
// would turn a request for "/router/test/my-box/8080/some%2Ffile" into an
// upstream request for "/some/file" — two segments instead of one,
// silently renaming whatever resource "some%2Ffile" actually named (a
// filename containing "/", URL-encoded to keep it within a single path
// segment, is exactly the kind of thing a browser-facing tool like
// code-server can be asked to serve). ParsePathRoute/resolveTarget fix
// this by working from r.URL.EscapedPath() and setting the outbound URL's
// RawPath alongside Path; this test proves the fix holds through the
// actual httputil.ReverseProxy hop, not just the parser in isolation
// (which pathroute_test.go already covers).
//
// Deliberately NOT behind the "integration" build tag, unlike its
// siblings in this package: it needs nothing beyond two in-process
// httptest servers, same as pathroute_test.go's table tests, and
// dev/tools/test-unit (the only Go test job wired into CI here — there is
// no separate integration presubmit) runs without -tags=integration. A
// regression this specific is worth keeping under the suite that
// actually runs.
func TestPathRoutingPreservesEncodedSlash(t *testing.T) {
	var (
		mu             sync.Mutex
		gotEscapedPath string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotEscapedPath = r.URL.EscapedPath()
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	cfg := config.Defaults()
	cfg.PathRoutingPrefix = "/router"
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"path-routed-uid": {PodIP: bu.Hostname(), SandboxName: "my-box", Namespace: "test"},
	}}
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	resp, err := http.Get(router.URL + "/router/test/my-box/" + bu.Port() + "/some%2Ffile")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status: got %d want 204", resp.StatusCode)
	}

	mu.Lock()
	got := gotEscapedPath
	mu.Unlock()
	const want = "/some%2Ffile"
	if got != want {
		t.Fatalf("backend saw escaped path %q, want %q (the encoded slash must survive the hop)", got, want)
	}
}

// Regression test for issue #2201: a header-routed "%2F" must survive the
// hop instead of being decoded into a double-slash that sandboxd then 307s.
func TestHeaderRoutingPreservesEncodedSlash(t *testing.T) {
	var (
		mu             sync.Mutex
		gotEscapedPath string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotEscapedPath = r.URL.EscapedPath()
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend: %v", err)
	}

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"header-routed-uid": {PodIP: bu.Hostname(), SandboxName: "my-box", Namespace: "test"},
	}}
	router := httptest.NewServer(NewHandler(Options{
		Config: &cfg,
		Cache:  lookup,
		Logger: logr.Discard(),
	}))
	defer router.Close()

	req, err := http.NewRequest(http.MethodGet, router.URL+"/v1/files/some%2Ffile", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(HeaderSandboxID, "my-box")
	req.Header.Set(HeaderSandboxNamespace, "test")
	req.Header.Set(HeaderSandboxUID, "header-routed-uid")
	req.Header.Set(HeaderSandboxPort, bu.Port())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status: got %d want 204", resp.StatusCode)
	}

	mu.Lock()
	got := gotEscapedPath
	mu.Unlock()
	const want = "/v1/files/some%2Ffile"
	if got != want {
		t.Fatalf("backend saw escaped path %q, want %q (the encoded slash must survive the hop)", got, want)
	}
}

// TestHeaderRoutingSandboxdFilePaths is the issue #2201 regression through a
// real sandboxd. Encoded separators used to be decoded into "//" or a dot
// segment, and sandboxd's ServeMux answered with 307. Literal separators,
// which the Python SDK sends after collapsing "." and "..", must reach the
// same files.
func TestHeaderRoutingSandboxdFilePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.txt":         "root file",
		"dir/child.txt": "child file",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	daemon, err := sandboxdserver.New(sandboxdserver.Options{RootDir: root, Log: logr.Discard()})
	if err != nil {
		t.Fatalf("new sandboxd: %v", err)
	}
	backend := httptest.NewServer(daemon.RESTHandler())
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	cfg := config.Defaults()
	cfg.AllowLoopbackPodIP = true
	lookup := &stubLookup{entries: map[types.UID]cache.Entry{
		"file-path-uid": {PodIP: backendURL.Hostname(), SandboxName: "my-box", Namespace: "test"},
	}}
	router := httptest.NewServer(NewHandler(Options{Config: &cfg, Cache: lookup, Logger: logr.Discard()}))
	defer router.Close()
	client := router.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	tests := []struct {
		name      string
		path      string
		status    int
		body      string
		listEntry string
	}{
		{name: "encoded root", path: "/v1/files/%2F", status: http.StatusOK, listEntry: "a.txt"},
		{name: "encoded absolute directory", path: "/v1/files/%2Fdir", status: http.StatusOK, listEntry: "child.txt"},
		{name: "encoded leading dot", path: "/v1/files/%2E%2Fa.txt", status: http.StatusOK, body: "root file"},
		{name: "encoded parent segment", path: "/v1/files/dir%2F%2E%2E%2Fa.txt", status: http.StatusOK, body: "root file"},
		{name: "encoded repeated slash", path: "/v1/files/dir%2F%2Fchild.txt", status: http.StatusOK, body: "child file"},
		{name: "literal root", path: "/v1/files/", status: http.StatusOK, listEntry: "a.txt"},
		{name: "literal directory", path: "/v1/files/dir", status: http.StatusOK, listEntry: "child.txt"},
		{name: "literal nested file", path: "/v1/files/dir/child.txt", status: http.StatusOK, body: "child file"},
		{name: "dot segment beside a slash", path: "/v1/files/%2E/a.txt", status: http.StatusOK, body: "root file"},
		{name: "parent segment beside a slash", path: "/v1/files/dir/%2E%2E/a.txt", status: http.StatusOK, body: "root file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, router.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(HeaderSandboxID, "my-box")
			req.Header.Set(HeaderSandboxNamespace, "test")
			req.Header.Set(HeaderSandboxUID, "file-path-uid")
			req.Header.Set(HeaderSandboxPort, backendURL.Port())
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request through router: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.status {
				t.Fatalf("status: got %d want %d, body %s", resp.StatusCode, test.status, got)
			}
			if test.listEntry != "" {
				var listing sandboxdserver.DirectoryListing
				if err := json.Unmarshal(got, &listing); err != nil {
					t.Fatalf("decode listing: %v body %s", err, got)
				}
				for _, entry := range listing.Entries {
					if entry.Name == test.listEntry {
						return
					}
				}
				t.Fatalf("listing lacks %q: %s", test.listEntry, got)
			}
			if string(got) != test.body {
				t.Fatalf("file content: got %q want %q", got, test.body)
			}
		})
	}
}
