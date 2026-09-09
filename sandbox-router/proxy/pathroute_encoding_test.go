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
	"net/url"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

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
