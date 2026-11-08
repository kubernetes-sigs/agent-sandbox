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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type recordedRequest struct {
	method      string
	path        string
	contentType string
	body        []byte
}

// newResizeTestSandbox points a real clientset at an httptest server so the
// /resize subresource request is exercised over real HTTP.
func newResizeTestSandbox(t *testing.T, handler http.Handler) (*Sandbox, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*rec = recordedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		handler.ServeHTTP(w, r)
	}))

	config := &rest.Config{Host: server.URL}
	coreClient, err := corev1client.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	sb := &Sandbox{
		k8s: &K8sHelper{
			CoreClient: coreClient,
			Log:        logr.Discard(),
		},
		sandboxName: "my-sandbox",
		podName:     "my-sandbox",
		opts:        Options{Namespace: "default"},
	}
	t.Cleanup(server.Close)
	return sb, rec
}

func resizeTestResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

func podJSON() string {
	return `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"my-sandbox","namespace":"default"}}`
}

const resizePath = "/api/v1/namespaces/default/pods/my-sandbox/resize"

func TestResizePodResources_PatchesResizeSubresource(t *testing.T) {
	desired := resizeTestResources()
	sb, rec := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, podJSON())
	}))

	if err := sb.ResizePodResources(t.Context(), "workspace", desired); err != nil {
		t.Fatalf("ResizePodResources: %v", err)
	}

	if rec.method != http.MethodPatch {
		t.Fatalf("expected PATCH, got %s", rec.method)
	}
	if rec.path != resizePath {
		t.Fatalf("expected resize subresource path %q, got %q", resizePath, rec.path)
	}
	if got := rec.contentType; got != "application/strategic-merge-patch+json" {
		t.Fatalf("expected strategic merge patch content type, got %q", got)
	}

	var patch struct {
		Spec struct {
			Containers []struct {
				Name      string                      `json:"name"`
				Resources corev1.ResourceRequirements `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.body, &patch); err != nil {
		t.Fatalf("unmarshal patch body: %v", err)
	}
	if len(patch.Spec.Containers) != 1 || patch.Spec.Containers[0].Name != "workspace" {
		t.Fatalf("unexpected patch containers: %s", rec.body)
	}
	got := patch.Spec.Containers[0].Resources
	if !got.Requests.Cpu().Equal(*desired.Requests.Cpu()) || !got.Limits.Memory().Equal(*desired.Limits.Memory()) {
		t.Fatalf("unexpected resources in patch: %s", rec.body)
	}
}

func TestResizePodResources_DoesNotTouchSandboxSpec(t *testing.T) {
	var paths []string
	sb, _ := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paths = append(paths, r.Method+" "+r.URL.Path)
		fmt.Fprint(w, podJSON())
	}))

	if err := sb.ResizePodResources(t.Context(), "workspace", resizeTestResources()); err != nil {
		t.Fatalf("ResizePodResources: %v", err)
	}
	for _, p := range paths {
		if p != "PATCH "+resizePath {
			t.Fatalf("expected only the resize subresource to be called, got %v", paths)
		}
	}
}

func TestResizePodResources_WithoutK8sHelper(t *testing.T) {
	sb := &Sandbox{}
	err := sb.ResizePodResources(t.Context(), "workspace", resizeTestResources())
	if err == nil {
		t.Fatal("expected error when K8sHelper is missing")
	}
}
