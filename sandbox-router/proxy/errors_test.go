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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSONError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSONError(rec, &Error{Status: http.StatusBadGateway, Detail: "Could not connect to the backend sandbox: my-box"})

	if got := rec.Code; got != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "my-box") {
		t.Errorf("detail should mention sandbox id; got %q", detail)
	}
	if len(body) != 1 {
		t.Errorf("body should have exactly the {detail} key; got %v", body)
	}
}

func TestErrorImplementsError(t *testing.T) {
	var err error = &Error{Status: 400, Detail: "boom"}
	if err.Error() != "boom" {
		t.Fatalf("Error() got %q want %q", err.Error(), "boom")
	}
}
