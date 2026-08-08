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

package management

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// k8sLabelValue matches valid Kubernetes label values (empty or up to 63 printable
// alphanumeric/.-_ characters, starting and ending with alphanumeric).
var k8sLabelValue = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9\-._]{0,61}[A-Za-z0-9])?)?$`)

// Handler is the HTTP handler for the /v1/ management API.
type Handler struct {
	client           *Client
	log              logr.Logger
	defaultNamespace string
}

// NewHandler wires up all /v1/sandboxes routes and returns the mux as an
// http.Handler. The caller should mount it at /v1/ on the top-level mux so
// that the catch-all proxy route is not affected.
func NewHandler(client *Client, log logr.Logger, defaultNamespace string) http.Handler {
	h := &Handler{client: client, log: log, defaultNamespace: defaultNamespace}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", h.create)
	mux.HandleFunc("GET /v1/sandboxes", h.list)
	mux.HandleFunc("GET /v1/sandboxes/{name}", h.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{name}", h.delete)
	mux.HandleFunc("GET /v1/sandboxes/{namespace}/{name}", h.getWithNS)
	mux.HandleFunc("DELETE /v1/sandboxes/{namespace}/{name}", h.deleteWithNS)
	return mux
}

// create handles POST /v1/sandboxes.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.WarmPool == "" {
		writeError(w, http.StatusBadRequest, "warmPool is required")
		return
	}
	if req.IdempotencyKey != "" && !k8sLabelValue.MatchString(req.IdempotencyKey) {
		writeError(w, http.StatusBadRequest, "idempotencyKey must be a valid Kubernetes label value (≤63 chars, alphanumeric/.-_)")
		return
	}
	if req.Namespace == "" {
		req.Namespace = h.defaultNamespace
	}

	claim, err := h.client.Create(r.Context(), &req)
	if err != nil {
		h.log.Error(err, "create SandboxClaim failed")
		writeError(w, http.StatusInternalServerError, "failed to create sandbox claim")
		return
	}
	writeJSON(w, http.StatusAccepted, h.client.toResponse(claim))
}

// list handles GET /v1/sandboxes.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = h.defaultNamespace
	}

	claimList, err := h.client.List(r.Context(), ns)
	if err != nil {
		h.log.Error(err, "list SandboxClaims failed")
		writeError(w, http.StatusInternalServerError, "failed to list sandbox claims")
		return
	}

	items := make([]SandboxResponse, 0, len(claimList.Items))
	for i := range claimList.Items {
		items = append(items, *h.client.toResponse(&claimList.Items[i]))
	}
	writeJSON(w, http.StatusOK, SandboxListResponse{Items: items, Total: len(items)})
}

// get handles GET /v1/sandboxes/{name}.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.getByNS(w, r, h.defaultNamespace, name)
}

// getWithNS handles GET /v1/sandboxes/{namespace}/{name}.
func (h *Handler) getWithNS(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	h.getByNS(w, r, ns, name)
}

func (h *Handler) getByNS(w http.ResponseWriter, r *http.Request, namespace, name string) {
	claim, err := h.client.Get(r.Context(), namespace, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "sandbox claim not found")
			return
		}
		h.log.Error(err, "get SandboxClaim failed", "namespace", namespace, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to get sandbox claim")
		return
	}
	writeJSON(w, http.StatusOK, h.client.toResponse(claim))
}

// delete handles DELETE /v1/sandboxes/{name}.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.deleteByNS(w, r, h.defaultNamespace, name)
}

// deleteWithNS handles DELETE /v1/sandboxes/{namespace}/{name}.
func (h *Handler) deleteWithNS(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	h.deleteByNS(w, r, ns, name)
}

func (h *Handler) deleteByNS(w http.ResponseWriter, r *http.Request, namespace, name string) {
	// Check existence first so we can return 404 correctly. The Delete method
	// swallows NotFound, so we need to check upfront.
	_, err := h.client.Get(r.Context(), namespace, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "sandbox claim not found")
			return
		}
		h.log.Error(err, "get SandboxClaim for delete failed", "namespace", namespace, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to delete sandbox claim")
		return
	}

	if err := h.client.Delete(r.Context(), namespace, name); err != nil {
		h.log.Error(err, "delete SandboxClaim failed", "namespace", namespace, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to delete sandbox claim")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers already sent; nothing useful we can do.
		return
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Status: status, Message: msg})
}
