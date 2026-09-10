/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// patchOp mirrors the unexported patchOperation shape in main.go. Value is
// left as json.RawMessage since it's a string in the "existing annotations
// map" patch but an object in the "create annotations map" patch.
type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

func newAdmissionReviewBody(t *testing.T, uid types.UID, namespace, name string, object []byte) []byte {
	t.Helper()
	ar := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1"},
		Request: &admissionv1.AdmissionRequest{
			UID:       uid,
			Namespace: namespace,
			Name:      name,
			Object:    runtime.RawExtension{Raw: object},
		},
	}
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("json.Marshal(AdmissionReview): %v", err)
	}
	return body
}

func doMutate(t *testing.T, body []byte) (*httptest.ResponseRecorder, admissionv1.AdmissionReview) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleMutate(rec, req)

	var got admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a valid AdmissionReview: %v\nbody: %s", err, rec.Body.String())
	}
	return rec, got
}

func TestHandleMutate_EmptyBody(t *testing.T) {
	rec, _ := doMutateRaw(t, nil)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty body") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "empty body")
	}
}

func TestHandleMutate_MalformedJSON(t *testing.T) {
	rec, _ := doMutateRaw(t, []byte("{not valid json"))
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not decode body") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "could not decode body")
	}
}

// doMutateRaw is like doMutate but for inputs that never make it to a valid
// AdmissionReview response (http.Error short-circuits with plain text).
func doMutateRaw(t *testing.T, body []byte) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleMutate(rec, req)
	return rec, rec.Body.Bytes()
}

func TestHandleMutate_MissingRequest(t *testing.T) {
	ar := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1"},
	}
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (admission responses are always HTTP 200)", rec.Code)
	}
	if got.Response == nil {
		t.Fatal("Response is nil")
	}
	if got.Response.Allowed {
		t.Error("Allowed = true, want false when request is missing")
	}
	if got.Response.Result == nil || got.Response.Result.Message != "request is missing" {
		t.Errorf("Result = %+v, want Message %q", got.Response.Result, "request is missing")
	}
}

func TestHandleMutate_MissingObject(t *testing.T) {
	body := newAdmissionReviewBody(t, "uid-1", "default", "my-claim", nil)

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got.Response.Allowed {
		t.Error("Allowed = true, want false when request object is missing")
	}
	if got.Response.Result == nil || got.Response.Result.Message != "request object is missing" {
		t.Errorf("Result = %+v, want Message %q", got.Response.Result, "request object is missing")
	}
}

func TestHandleMutate_UnparsableObject_FailsOpen(t *testing.T) {
	// The object field is valid JSON but not a JSON object (a bare string),
	// so json.Unmarshal into map[string]interface{} fails with a type
	// error. failurePolicy: Ignore means the webhook must admit rather than
	// reject in this case.
	body := newAdmissionReviewBody(t, "uid-1", "default", "my-claim", []byte(`"not-an-object"`))

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !got.Response.Allowed {
		t.Error("Allowed = false, want true (fail open on unparsable object)")
	}
	if len(got.Response.Patch) != 0 {
		t.Errorf("Patch = %s, want no patch", got.Response.Patch)
	}
}

func TestHandleMutate_AddsAnnotationsMapWhenMissing(t *testing.T) {
	object := `{"metadata":{"name":"my-claim"}}`
	body := newAdmissionReviewBody(t, "uid-1", "default", "my-claim", []byte(object))

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !got.Response.Allowed {
		t.Error("Allowed = false, want true")
	}
	if got.Response.PatchType == nil || *got.Response.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("PatchType = %v, want JSONPatch", got.Response.PatchType)
	}

	var patches []patchOp
	if err := json.Unmarshal(got.Response.Patch, &patches); err != nil {
		t.Fatalf("patch is not valid JSON: %v (%s)", err, got.Response.Patch)
	}
	if len(patches) != 1 {
		t.Fatalf("got %d patch ops, want 1: %+v", len(patches), patches)
	}
	p := patches[0]
	if p.Op != "add" || p.Path != "/metadata/annotations" {
		t.Errorf("patch op = %+v, want op=add path=/metadata/annotations", p)
	}
	var value map[string]string
	if err := json.Unmarshal(p.Value, &value); err != nil {
		t.Fatalf("patch value is not an object: %v (%s)", err, p.Value)
	}
	assertRecentRFC3339Nano(t, value[annotationKey])
}

func TestHandleMutate_AddsKeyToExistingAnnotationsMap(t *testing.T) {
	object := `{"metadata":{"name":"my-claim","annotations":{"foo":"bar"}}}`
	body := newAdmissionReviewBody(t, "uid-1", "default", "my-claim", []byte(object))

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var patches []patchOp
	if err := json.Unmarshal(got.Response.Patch, &patches); err != nil {
		t.Fatalf("patch is not valid JSON: %v (%s)", err, got.Response.Patch)
	}
	if len(patches) != 1 {
		t.Fatalf("got %d patch ops, want 1: %+v", len(patches), patches)
	}
	p := patches[0]
	wantPath := "/metadata/annotations/agents.x-k8s.io~1webhook-first-observed-at"
	if p.Op != "add" || p.Path != wantPath {
		t.Errorf("patch op = %+v, want op=add path=%s", p, wantPath)
	}
	var value string
	if err := json.Unmarshal(p.Value, &value); err != nil {
		t.Fatalf("patch value is not a string: %v (%s)", err, p.Value)
	}
	assertRecentRFC3339Nano(t, value)
}

func TestHandleMutate_NoPatchWhenAnnotationAlreadyPresent(t *testing.T) {
	object := `{"metadata":{"name":"my-claim","annotations":{"agents.x-k8s.io/webhook-first-observed-at":"2026-01-01T00:00:00Z"}}}`
	body := newAdmissionReviewBody(t, "uid-1", "default", "my-claim", []byte(object))

	rec, got := doMutate(t, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !got.Response.Allowed {
		t.Error("Allowed = false, want true")
	}
	if len(got.Response.Patch) != 0 {
		t.Errorf("Patch = %s, want no patch (annotation already present, should be idempotent)", got.Response.Patch)
	}
	if got.Response.PatchType != nil {
		t.Errorf("PatchType = %v, want nil", got.Response.PatchType)
	}
}

func TestHandleMutate_PreservesUIDAndTypeMeta(t *testing.T) {
	object := `{"metadata":{"name":"my-claim"}}`
	body := newAdmissionReviewBody(t, "abc-123-uid", "team-a", "my-claim", []byte(object))

	_, got := doMutate(t, body)
	if got.Response.UID != types.UID("abc-123-uid") {
		t.Errorf("Response.UID = %q, want %q", got.Response.UID, "abc-123-uid")
	}
	if got.Kind != "AdmissionReview" || got.APIVersion != "admission.k8s.io/v1" {
		t.Errorf("TypeMeta = %+v, want it echoed from the request", got.TypeMeta)
	}
}

func assertRecentRFC3339Nano(t *testing.T, ts string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339Nano: %v", ts, err)
	}
	if since := time.Since(parsed); since < 0 || since > time.Minute {
		t.Errorf("timestamp %q is not close to now (delta %v)", ts, since)
	}
}
