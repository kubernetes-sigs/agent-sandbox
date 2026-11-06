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
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"

	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
)

func TestEncodeTrailerFrame(t *testing.T) {
	payload := []byte("grpc-status:0\r\n")
	frame := encodeTrailerFrame(payload)
	if frame[0] != grpcWebTrailerFlag {
		t.Fatalf("flag byte = %#x, want %#x", frame[0], grpcWebTrailerFlag)
	}
	if got := binary.BigEndian.Uint32(frame[1:5]); got != uint32(len(payload)) {
		t.Fatalf("length = %d, want %d", got, len(payload))
	}
	if !bytes.Equal(frame[5:], payload) {
		t.Fatalf("payload = %q, want %q", frame[5:], payload)
	}
}

func TestTrailerFrameFromHeaders(t *testing.T) {
	h := http.Header{}
	// The transport canonicalizes keys; the frame must carry lower-case
	// keys per the grpc-web spec.
	h.Set("Grpc-Status", "0")
	h.Set("Grpc-Message", "done%20ok")
	h.Add("X-Custom-Meta", "v1")
	h.Add("X-Custom-Meta", "v2")

	payload := trailerFramePayload(h)

	// grpc-status first, grpc-message second, remaining keys sorted;
	// every line CRLF-terminated; multi-value keys repeat the line.
	want := "grpc-status:0\r\ngrpc-message:done%20ok\r\nx-custom-meta:v1\r\nx-custom-meta:v2\r\n"
	if string(payload) != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

func TestTrailerFrameFromHeadersSkipsEmpty(t *testing.T) {
	if payload := trailerFramePayload(http.Header{}); len(payload) != 0 {
		t.Fatalf("payload = %q, want empty", payload)
	}
}

func TestWriteGRPCWebError(t *testing.T) {
	labels := &observability.Labels{}
	r := httptest.NewRequest(http.MethodPost, "/svc/Method", nil).
		WithContext(observability.WithLabels(context.Background(), labels))
	w := httptest.NewRecorder()

	writeGRPCWebError(w, r, codes.PermissionDenied, "not allowed here")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeGRPCWeb {
		t.Fatalf("content-type = %q, want %q", ct, contentTypeGRPCWeb)
	}
	body := w.Body.Bytes()
	if body[0] != grpcWebTrailerFlag {
		t.Fatalf("body is not a trailer frame: flag %#x", body[0])
	}
	if got := binary.BigEndian.Uint32(body[1:5]); got != uint32(len(body)-5) {
		t.Fatalf("frame length = %d, body payload = %d", got, len(body)-5)
	}
	want := "grpc-status:7\r\ngrpc-message:not%20allowed%20here\r\n"
	if string(body[5:]) != want {
		t.Fatalf("payload = %q, want %q", body[5:], want)
	}
	if w.Header().Get("Content-Length") != "" {
		// httptest records the resolved Content-Length here; require the
		// handler to have set it so HTTP/1.1 clients can frame the body.
		if cl := w.Result().ContentLength; cl != int64(len(body)) {
			t.Fatalf("content-length = %d, want %d", cl, len(body))
		}
	}
	if labels.GRPCStatusCode != "7" {
		t.Fatalf("recorded status = %q, want 7", labels.GRPCStatusCode)
	}
}
