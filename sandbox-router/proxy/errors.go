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
)

// Error carries an HTTP status code and a human-readable detail. It is
// serialized as `{"detail": "..."}` to match the Python router's error shape
// so that existing clients parsing those bodies keep working.
type Error struct {
	Status int
	Detail string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return e.Detail
}

// errorBody is the wire shape of an Error response. Kept private so the
// JSON contract is owned solely by WriteJSONError.
type errorBody struct {
	Detail string `json:"detail"`
}

// WriteJSONError writes e to w using the Python router's response shape.
// The Content-Type is set to application/json. Encoding errors are silently
// dropped — the status code has already been written and there is nothing
// useful to do.
func WriteJSONError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Detail: e.Detail})
}
