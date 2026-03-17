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

package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	fakeextensions "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

func sandboxHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/execute":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ExecutionResult{Stdout: "hello", ExitCode: 0})
		case strings.HasPrefix(r.URL.Path, "/download/"):
			w.Write([]byte("file-data"))
		case r.URL.Path == "/upload":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/list/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]FileEntry{{Name: "a.txt", Type: FileTypeFile}})
		case strings.HasPrefix(r.URL.Path, "/exists/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"exists": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTracedTestClient(t *testing.T, tp *sdktrace.TracerProvider) (*SandboxClient, *fakeagents.Clientset, *fakeextensions.Clientset, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(sandboxHTTPHandler())
	t.Cleanup(srv.Close)

	opts := Options{
		TemplateName:        "test-template",
		APIURL:              srv.URL,
		TracerProvider:      tp,
		TraceServiceName:    "test-svc",
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	c := newClientFromInterfaces(opts, agentsCS.AgentsV1alpha1(), extensionsCS.ExtensionsV1alpha1(), nil, nil, nil, nil)

	return c, agentsCS, extensionsCS, srv
}

// setupTracedWatch wires a fake watcher that sends the sandbox once
// the claim is created, and captures the created claim for inspection.
func setupTracedWatch(agentsCS *fakeagents.Clientset, extensionsCS *fakeextensions.Clientset) (*watch.FakeWatcher, **extv1alpha1.SandboxClaim) {
	fakeWatcher := watch.NewFake()
	agentsCS.PrependWatchReactor("sandboxes", ktesting.DefaultWatchReactor(fakeWatcher, nil))

	var captured *extv1alpha1.SandboxClaim
	extensionsCS.PrependReactor("create", "sandboxclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(ktesting.CreateAction); ok {
			if claim, ok := ca.GetObject().(*extv1alpha1.SandboxClaim); ok {
				captured = claim.DeepCopy()

				sb := &sandboxv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      claim.Name,
						Namespace: claim.Namespace,
						Annotations: map[string]string{
							PodNameAnnotation: claim.Name + "-pod",
						},
					},
					Status: sandboxv1alpha1.SandboxStatus{
						Conditions: []metav1.Condition{{
							Type:   string(sandboxv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						}},
					},
				}
				// Must use goroutine: FakeWatcher uses an unbuffered channel.
				go fakeWatcher.Add(sb)
			}
		}
		return false, nil, nil
	})

	return fakeWatcher, &captured
}

func TestTracingLifecycleAndOperations(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	c, agentsCS, extensionsCS, _ := newTracedTestClient(t, tp)
	_, capturedClaim := setupTracedWatch(agentsCS, extensionsCS)

	ctx := context.Background()

	// Open
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Run
	result, err := c.Run(ctx, "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}

	// Write
	if err := c.Write(ctx, "/tmp/test.txt", []byte("content")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read
	data, err := c.Read(ctx, "/tmp/test.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "file-data" {
		t.Fatalf("unexpected read data: %s", data)
	}

	// List
	entries, err := c.List(ctx, "/tmp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("unexpected entries count: %d", len(entries))
	}

	// Exists
	exists, err := c.Exists(ctx, "/tmp/test.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}

	// Close
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify spans
	spans := exporter.GetSpans()
	spanByName := make(map[string]tracetest.SpanStub)
	for _, s := range spans {
		spanByName[s.Name] = s
	}

	expectedSpans := []string{
		"test-svc.lifecycle",
		"test-svc.create_claim",
		"test-svc.wait_for_sandbox_ready",
		"test-svc.run",
		"test-svc.write",
		"test-svc.read",
		"test-svc.list",
		"test-svc.exists",
	}
	for _, name := range expectedSpans {
		if _, ok := spanByName[name]; !ok {
			t.Errorf("missing expected span: %s (got: %v)", name, spanNames(spans))
		}
	}

	// Verify lifecycle is parent of all operation spans.
	lifecycle, ok := spanByName["test-svc.lifecycle"]
	if !ok {
		t.Fatal("lifecycle span not found")
	}
	lifecycleSpanID := lifecycle.SpanContext.SpanID()
	for _, name := range expectedSpans[1:] {
		child, ok := spanByName[name]
		if !ok {
			continue
		}
		if child.Parent.SpanID() != lifecycleSpanID {
			t.Errorf("span %s parent=%s, want lifecycle=%s", name, child.Parent.SpanID(), lifecycleSpanID)
		}
	}

	// Verify Run attributes
	assertSpanAttr(t, spanByName["test-svc.run"], "sandbox.command", "echo hello")
	assertSpanAttrInt(t, spanByName["test-svc.run"], "sandbox.exit_code", 0)

	// Verify Write attributes
	assertSpanAttr(t, spanByName["test-svc.write"], "sandbox.file.path", "/tmp/test.txt")
	assertSpanAttrInt(t, spanByName["test-svc.write"], "sandbox.file.size", 7) // len("content")

	// Verify Read attributes
	assertSpanAttr(t, spanByName["test-svc.read"], "sandbox.file.path", "/tmp/test.txt")
	assertSpanAttrInt(t, spanByName["test-svc.read"], "sandbox.file.size", 9) // len("file-data")

	// Verify List attributes
	assertSpanAttr(t, spanByName["test-svc.list"], "sandbox.file.path", "/tmp")
	assertSpanAttrInt(t, spanByName["test-svc.list"], "sandbox.file.count", 1)

	// Verify Exists attributes
	assertSpanAttr(t, spanByName["test-svc.exists"], "sandbox.file.path", "/tmp/test.txt")
	assertSpanAttrBool(t, spanByName["test-svc.exists"], "sandbox.file.exists", true)

	// Verify claim name attribute
	if s, ok := spanByName["test-svc.create_claim"]; ok {
		found := false
		for _, a := range s.Attributes {
			if string(a.Key) == "sandbox.claim.name" && a.Value.AsString() != "" {
				found = true
			}
		}
		if !found {
			t.Error("create_claim span missing sandbox.claim.name attribute")
		}
	}

	// Verify trace context annotation on claim
	if *capturedClaim != nil {
		tc, ok := (*capturedClaim).Annotations["opentelemetry.io/trace-context"]
		if !ok || tc == "" {
			t.Error("claim missing opentelemetry.io/trace-context annotation")
		} else {
			var parsed map[string]string
			if err := json.Unmarshal([]byte(tc), &parsed); err != nil {
				t.Errorf("trace context annotation is not valid JSON: %v", err)
			} else if _, ok := parsed["traceparent"]; !ok {
				t.Error("trace context annotation missing traceparent")
			}
		}
	}
}

func TestTracingNoopWithoutProvider(t *testing.T) {
	srv := httptest.NewServer(sandboxHTTPHandler())
	t.Cleanup(srv.Close)

	opts := Options{
		TemplateName:        "test-template",
		APIURL:              srv.URL,
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	c := newClientFromInterfaces(opts, agentsCS.AgentsV1alpha1(), extensionsCS.ExtensionsV1alpha1(), nil, nil, nil, nil)

	sb := readySandbox("test-claim", "default")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := c.Run(ctx, "echo noop"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestTracingErrorRecording(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	// Server that returns 400 for Run
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	t.Cleanup(srv.Close)

	opts := Options{
		TemplateName:        "test-template",
		APIURL:              srv.URL,
		TracerProvider:      tp,
		TraceServiceName:    "test-svc",
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()

	agentsCS := fakeagents.NewSimpleClientset()
	extensionsCS := fakeextensions.NewSimpleClientset()
	c := newClientFromInterfaces(opts, agentsCS.AgentsV1alpha1(), extensionsCS.ExtensionsV1alpha1(), nil, nil, nil, nil)

	sb := readySandbox("test-claim", "default")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Run should fail
	_, err := c.Run(ctx, "fail")
	if err == nil {
		t.Fatal("expected Run to fail")
	}

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := exporter.GetSpans()
	for _, s := range spans {
		if s.Name == "test-svc.run" {
			if s.Status.Code == 0 { // codes.Unset
				t.Error("run span should have error status")
			}
			if len(s.Events) == 0 {
				t.Error("run span should have error event")
			}
			return
		}
	}
	t.Error("run span not found")
}

// --- helpers ---

func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

func assertSpanAttr(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			if a.Value.AsString() != want {
				t.Errorf("span %s: attr %s = %q, want %q", span.Name, key, a.Value.AsString(), want)
			}
			return
		}
	}
	t.Errorf("span %s: missing attribute %s", span.Name, key)
}

func assertSpanAttrInt(t *testing.T, span tracetest.SpanStub, key string, want int64) {
	t.Helper()
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			if a.Value.AsInt64() != want {
				t.Errorf("span %s: attr %s = %d, want %d", span.Name, key, a.Value.AsInt64(), want)
			}
			return
		}
	}
	t.Errorf("span %s: missing attribute %s", span.Name, key)
}

func assertSpanAttrBool(t *testing.T, span tracetest.SpanStub, key string, want bool) {
	t.Helper()
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			if a.Value.AsBool() != want {
				t.Errorf("span %s: attr %s = %v, want %v", span.Name, key, a.Value.AsBool(), want)
			}
			return
		}
	}
	t.Errorf("span %s: missing attribute %s", span.Name, key)
}
