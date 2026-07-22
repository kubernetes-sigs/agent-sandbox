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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const annotationKey = "agents.x-k8s.io/webhook-first-observed-at"

func main() {
	http.HandleFunc("/mutate", handleMutate)
	fmt.Println("Starting webhook server on :8443...")
	// GKE requires HTTPS for webhooks. You must provide cert and key files.
	// For testing, you can generate self-signed certs.
	if err := http.ListenAndServeTLS(":8443", "/etc/webhook/certs/tls.crt", "/etc/webhook/certs/tls.key", nil); err != nil {
		panic(err)
	}
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("could not read body: %v", err), http.StatusBadRequest)
			return
		}
		body = data
	}

	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// AdmissionReview has standard JSON tags, so json.Unmarshal is sufficient
	// and avoids needing a runtime scheme with the type registered.
	ar := admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, &ar); err != nil {
		http.Error(w, fmt.Sprintf("could not decode body: %v", err), http.StatusBadRequest)
		return
	}

	// Create Response
	arResponse := admissionv1.AdmissionReview{
		TypeMeta: ar.TypeMeta,
		Response: &admissionv1.AdmissionResponse{
			Allowed: true,
		},
	}

	var rawObj map[string]interface{}
	hasAnnotation := false
	hasAnnotationsMap := false

	if ar.Request == nil {
		arResponse.Response.Allowed = false
		arResponse.Response.Result = &metav1.Status{
			Message: "request is missing",
		}
		goto writeResponse
	}

	arResponse.Response.UID = ar.Request.UID

	if len(ar.Request.Object.Raw) == 0 {
		arResponse.Response.Allowed = false
		arResponse.Response.Result = &metav1.Status{
			Message: "request object is missing",
		}
		goto writeResponse
	}

	// Check if annotation already exists. On failure, admit without mutating and
	// surface the error in the response; failing the HTTP request here would count
	// as a webhook call failure, which failurePolicy: Ignore silently drops.
	if err := json.Unmarshal(ar.Request.Object.Raw, &rawObj); err != nil {
		log.Printf("could not unmarshal raw object, skipping mutation: %v", err)
		arResponse.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("could not unmarshal raw object, skipping mutation: %v", err),
		}
		goto writeResponse
	}
	if metadata, ok := rawObj["metadata"].(map[string]interface{}); ok {
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			hasAnnotationsMap = true
			if _, exists := annotations[annotationKey]; exists {
				hasAnnotation = true
			}
		}
	}

	if !hasAnnotation {
		// Create JSON Patch to add annotation
		now := time.Now().Format(time.RFC3339Nano)

		type patchOperation struct {
			Op    string      `json:"op"`
			Path  string      `json:"path"`
			Value interface{} `json:"value,omitempty"`
		}

		var patches []patchOperation
		if hasAnnotationsMap {
			// Path /metadata/annotations exists, add specific key
			patches = []patchOperation{
				{
					Op:    "add",
					Path:  "/metadata/annotations/agents.x-k8s.io~1webhook-first-observed-at",
					Value: now,
				},
			}
		} else {
			// Path /metadata/annotations does not exist, create it with the key
			patches = []patchOperation{
				{
					Op:    "add",
					Path:  "/metadata/annotations",
					Value: map[string]string{"agents.x-k8s.io/webhook-first-observed-at": now},
				},
			}
		}

		patchBytes, err := json.Marshal(patches)
		if err != nil {
			// Admit without mutating rather than failing the webhook call.
			log.Printf("could not encode patch, skipping mutation: %v", err)
			arResponse.Response.Result = &metav1.Status{
				Message: fmt.Sprintf("could not encode patch, skipping mutation: %v", err),
			}
			goto writeResponse
		}

		arResponse.Response.Patch = patchBytes
		patchType := admissionv1.PatchTypeJSONPatch
		arResponse.Response.PatchType = &patchType

		log.Printf("Injected %s timestamp for SandboxClaim %s/%s", annotationKey, ar.Request.Namespace, ar.Request.Name)
	}

writeResponse:
	resp, err := json.Marshal(arResponse)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}
