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

package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
)

// capturedLine is the last "request" log line recorded by capturingSink.
type capturedLine struct {
	msg        string
	keyAndVals []interface{}
}

func (c *capturedLine) value(key string) (interface{}, bool) {
	for i := 0; i+1 < len(c.keyAndVals); i += 2 {
		if k, ok := c.keyAndVals[i].(string); ok && k == key {
			return c.keyAndVals[i+1], true
		}
	}
	return nil, false
}

func newCapturingLogger(dst *capturedLine) logr.Logger {
	return logr.New(&capturingSink{dst: dst})
}

// capturingSink is a minimal logr.LogSink that records the last Info()
// call, so the test can assert on individual structured fields directly
// instead of parsing a formatted log line back apart.
type capturingSink struct {
	dst *capturedLine
}

func (s *capturingSink) Init(logr.RuntimeInfo)                  {}
func (s *capturingSink) Enabled(int) bool                       { return true }
func (s *capturingSink) WithName(string) logr.LogSink           { return s }
func (s *capturingSink) WithValues(...interface{}) logr.LogSink { return s }
func (s *capturingSink) Error(error, string, ...interface{})    {}
func (s *capturingSink) Info(_ int, msg string, kv ...interface{}) {
	*s.dst = capturedLine{msg: msg, keyAndVals: kv}
}

// TestAccessLogMiddleware_UsesResolvedIdentityNotHeaders is the regression
// test for the bug caught in review: sandbox_id/sandbox_namespace log
// fields used to be read straight from the X-Sandbox-* request headers,
// which a path-routed request (browser traffic reaching the router via
// --path-routing-prefix, see ParsePathRoute) never sets at all — so every
// such request was logged with an empty sandbox identity, making exactly
// that traffic unsearchable. The inner handler here sets Labels directly
// (exactly what the proxy Handler's ServeHTTP does once it resolves a
// Target, from either input) with NO X-Sandbox-* header on the request at
// all, simulating a path-routed request end to end.
func TestAccessLogMiddleware_UsesResolvedIdentityNotHeaders(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels := LabelsFromContext(r.Context())
		if labels == nil {
			t.Fatal("inner handler: no *Labels attached by AccessLogMiddleware")
		}
		labels.SandboxID = "path-routed-box"
		labels.SandboxNamespace = "poc-agent-sandbox"
		w.WriteHeader(http.StatusOK)
	})

	var line capturedLine
	mw := AccessLogMiddleware(newCapturingLogger(&line), nil)
	req := httptest.NewRequest(http.MethodGet, "/router/poc-agent-sandbox/path-routed-box/8080/", nil)
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)

	if line.msg != "request" {
		t.Fatalf("expected a logged \"request\" line, got %+v", line)
	}
	if v, ok := line.value("sandbox_id"); !ok || v != "path-routed-box" {
		t.Errorf("sandbox_id field: got %v (present=%v), want %q", v, ok, "path-routed-box")
	}
	if v, ok := line.value("sandbox_namespace"); !ok || v != "poc-agent-sandbox" {
		t.Errorf("sandbox_namespace field: got %v (present=%v), want %q", v, ok, "poc-agent-sandbox")
	}
}
