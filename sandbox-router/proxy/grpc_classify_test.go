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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		contentType string
		protoMajor  int
		want        protocolKind
	}{
		{"native grpc over h2", http.MethodPost, "application/grpc", 2, protoGRPC},
		{"native grpc codec over h2", http.MethodPost, "application/grpc+proto", 2, protoGRPC},
		// Native gRPC clients always speak HTTP/2; keeping the gate stops
		// malformed HTTP/1.1 requests from entering the gRPC pipeline.
		{"native grpc over h1 stays http", http.MethodPost, "application/grpc", 1, protoHTTP},
		{"grpc-web over h1", http.MethodPost, "application/grpc-web", 1, protoGRPCWeb},
		{"grpc-web over h2", http.MethodPost, "application/grpc-web", 2, protoGRPCWeb},
		{"grpc-web proto codec", http.MethodPost, "application/grpc-web+proto", 1, protoGRPCWeb},
		{"grpc-web with charset param", http.MethodPost, "application/grpc-web; charset=utf-8", 1, protoGRPCWeb},
		{"grpc-web-text rejected", http.MethodPost, "application/grpc-web-text", 1, protoGRPCWebReject},
		{"grpc-web-text+proto rejected", http.MethodPost, "application/grpc-web-text+proto", 1, protoGRPCWebReject},
		{"grpc-web+json rejected", http.MethodPost, "application/grpc-web+json", 1, protoGRPCWebReject},
		{"json is http", http.MethodPost, "application/json", 1, protoHTTP},
		{"empty content type is http", http.MethodPost, "", 1, protoHTTP},
		{"get with grpc-web type is http", http.MethodGet, "application/grpc-web", 1, protoHTTP},
		{"malformed content type is http", http.MethodPost, "application/grpc-web; charset=", 1, protoHTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/svc/Method", nil)
			r.ProtoMajor = tc.protoMajor
			r.ProtoMinor = 0
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			if got := classifyRequest(r); got != tc.want {
				t.Fatalf("classifyRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProtocolKindIsGRPCish(t *testing.T) {
	if protoHTTP.isGRPCish() {
		t.Fatal("protoHTTP must not be grpc-ish")
	}
	for _, kind := range []protocolKind{protoGRPC, protoGRPCWeb} {
		if !kind.isGRPCish() {
			t.Fatalf("%v must be grpc-ish", kind)
		}
	}
}

func TestWriteProxyErrorGRPCWebFormat(t *testing.T) {
	// Every router-level error exit must reach a grpc-web caller as a
	// trailers-only frame, not JSON: a browser gRPC-Web client cannot
	// parse the JSON error shape.
	for _, tc := range []struct {
		status     int
		wantStatus string
	}{
		{http.StatusBadRequest, "3"},           // InvalidArgument
		{http.StatusUnauthorized, "16"},        // Unauthenticated
		{http.StatusForbidden, "7"},            // PermissionDenied
		{http.StatusBadGateway, "14"},          // Unavailable
		{http.StatusInternalServerError, "13"}, // Internal
	} {
		r := httptest.NewRequest(http.MethodPost, "/svc/Method", nil)
		w := httptest.NewRecorder()
		writeProxyError(w, r, protoGRPCWeb, &Error{Status: tc.status, Detail: "boom"})
		body := w.Body.String()
		if !strings.Contains(body, "grpc-status:"+tc.wantStatus+"\r\n") {
			t.Fatalf("status %d: body = %q, want grpc-status:%s", tc.status, body, tc.wantStatus)
		}
		if ct := w.Header().Get("Content-Type"); ct != contentTypeGRPCWeb {
			t.Fatalf("status %d: content-type = %q", tc.status, ct)
		}
	}
}
