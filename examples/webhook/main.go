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
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	universalDeserializer = serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
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
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	ar := admissionv1.AdmissionReview{}
	if _, _, err := universalDeserializer.Decode(body, nil, &ar); err != nil {
		http.Error(w, fmt.Sprintf("could not decode body: %v", err), http.StatusBadRequest)
		return
	}

	// Create Response
	arResponse := admissionv1.AdmissionReview{
		TypeMeta: ar.TypeMeta,
		Response: &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: true,
		},
	}

	// Check if annotation already exists
	var rawObj map[string]interface{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &rawObj); err != nil {
		http.Error(w, fmt.Sprintf("could not unmarshal raw object: %v", err), http.StatusBadRequest)
		return
	}

	hasAnnotation := false
	hasAnnotationsMap := false
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
		
		var patchStr string
		if hasAnnotationsMap {
			// Path /metadata/annotations exists, add specific key
			patchStr = fmt.Sprintf(`[{"op": "add", "path": "/metadata/annotations/agents.x-k8s.io~1webhook-first-observed-at", "value": "%s"}]`, now)
		} else {
			// Path /metadata/annotations does not exist, create it with the key
			patchStr = fmt.Sprintf(`[{"op": "add", "path": "/metadata/annotations", "value": {"agents.x-k8s.io/webhook-first-observed-at": "%s"}}]`, now)
		}
		
		arResponse.Response.Patch = []byte(patchStr)
		patchType := admissionv1.PatchTypeJSONPatch
		arResponse.Response.PatchType = &patchType
	}

	resp, err := json.Marshal(arResponse)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

