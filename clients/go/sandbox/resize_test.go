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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	agentsclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
)

type recordedRequest struct {
	method      string
	path        string
	contentType string
	body        []byte
}

// newResizeTestSandbox points real clientsets at an httptest server so the
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
	agentsCS, err := agentsclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	sb := &Sandbox{
		k8s: &K8sHelper{
			CoreClient:   coreClient,
			AgentsClient: agentsCS.AgentsV1beta1(),
			Log:          logr.Discard(),
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

// resizeTestSandboxJSON renders a two-container Sandbox: the resized
// "workspace" container carries an image and resourceVersion so tests can
// assert the template sync preserves fields other than resources.
func resizeTestSandboxJSON(workspaceResources corev1.ResourceRequirements) string {
	sb := &sandboxv1beta1.Sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: "my-sandbox", Namespace: "default", ResourceVersion: "12345"},
	}
	sb.Spec.PodTemplate.Spec.Containers = []corev1.Container{
		{Name: "workspace", Image: "workspace:latest", Resources: workspaceResources},
		{Name: "sidecar", Image: "sidecar:latest"},
	}
	out, err := json.Marshal(sb)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func podJSON() string {
	return `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"my-sandbox","namespace":"default"}}`
}

const resizePath = "/api/v1/namespaces/default/pods/my-sandbox/resize"
const sandboxPath = "/apis/agents.x-k8s.io/v1beta1/namespaces/default/sandboxes/my-sandbox"

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

func TestResizePodResources_NoTemplateSyncByDefault(t *testing.T) {
	sb, rec := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, podJSON())
	}))

	if err := sb.ResizePodResources(t.Context(), "workspace", resizeTestResources()); err != nil {
		t.Fatalf("ResizePodResources: %v", err)
	}
	if rec.path != resizePath {
		t.Fatalf("expected only the resize subresource to be called, got %q", rec.path)
	}
}

func TestResizePodResources_SyncsSandboxTemplate(t *testing.T) {
	desired := resizeTestResources()
	var paths []string
	sb, rec := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == sandboxPath:
			fmt.Fprint(w, resizeTestSandboxJSON(desired))
		case r.Method == http.MethodGet && r.URL.Path == sandboxPath:
			fmt.Fprint(w, resizeTestSandboxJSON(corev1.ResourceRequirements{}))
		default:
			fmt.Fprint(w, podJSON())
		}
	}))

	if err := sb.ResizePodResources(t.Context(), "workspace", desired, WithTemplateSync()); err != nil {
		t.Fatalf("ResizePodResources: %v", err)
	}

	var sawResize, sawTemplate bool
	for _, p := range paths {
		switch p {
		case "PATCH " + resizePath:
			sawResize = true
		case "PATCH " + sandboxPath:
			sawTemplate = true
		}
	}
	if !sawResize || !sawTemplate {
		t.Fatalf("expected resize and template patches, got %v", paths)
	}

	var patch struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			PodTemplate struct {
				Spec struct {
					Containers []corev1.Container `json:"containers"`
				} `json:"spec"`
			} `json:"podTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.body, &patch); err != nil {
		t.Fatalf("unmarshal template patch: %v", err)
	}
	if patch.Metadata.ResourceVersion != "12345" {
		t.Fatalf("expected resourceVersion in template patch: %s", rec.body)
	}
	containers := patch.Spec.PodTemplate.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("expected both containers preserved in template patch: %s", rec.body)
	}
	var workspace, sidecar *corev1.Container
	for i := range containers {
		switch containers[i].Name {
		case "workspace":
			workspace = &containers[i]
		case "sidecar":
			sidecar = &containers[i]
		}
	}
	if workspace == nil || sidecar == nil {
		t.Fatalf("expected workspace and sidecar in template patch: %s", rec.body)
	}
	if workspace.Image != "workspace:latest" {
		t.Fatalf("expected workspace image preserved, got %q", workspace.Image)
	}
	if !workspace.Resources.Requests.Cpu().Equal(*desired.Requests.Cpu()) {
		t.Fatalf("unexpected workspace resources: %s", rec.body)
	}
	if sidecar.Image != "sidecar:latest" {
		t.Fatalf("expected sidecar untouched, got %+v", sidecar)
	}
}

func TestResizePodResources_SkipsTemplatePatchWhenUnchanged(t *testing.T) {
	desired := resizeTestResources()
	var paths []string
	sb, _ := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == sandboxPath {
			fmt.Fprint(w, resizeTestSandboxJSON(desired))
			return
		}
		fmt.Fprint(w, podJSON())
	}))

	if err := sb.ResizePodResources(t.Context(), "workspace", desired, WithTemplateSync()); err != nil {
		t.Fatalf("ResizePodResources: %v", err)
	}
	for _, p := range paths {
		if p == "PATCH "+sandboxPath {
			t.Fatalf("template patched despite matching resources: %v", paths)
		}
	}
}

func TestResizePodResources_TemplateMissingContainer(t *testing.T) {
	desired := resizeTestResources()
	sb, _ := newResizeTestSandbox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == sandboxPath {
			fmt.Fprint(w, resizeTestSandboxJSON(corev1.ResourceRequirements{}))
			return
		}
		fmt.Fprint(w, podJSON())
	}))

	err := sb.ResizePodResources(t.Context(), "no-such-container", desired, WithTemplateSync())
	if err == nil {
		t.Fatal("expected error for missing template container")
	}
}

func TestResizePodResources_WithoutK8sHelper(t *testing.T) {
	sb := &Sandbox{}
	err := sb.ResizePodResources(t.Context(), "workspace", resizeTestResources())
	if err == nil {
		t.Fatal("expected error when K8sHelper is missing")
	}
}
