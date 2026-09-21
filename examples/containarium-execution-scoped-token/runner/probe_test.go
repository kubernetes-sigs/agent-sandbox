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

import "testing"

func TestHeaderValue(t *testing.T) {
	headers := "Content-Type: application/json\r\nX-Foo: bar\r\n"

	if got := headerValue(headers, "Content-Type"); got != "application/json" {
		t.Errorf("headerValue(Content-Type) = %q, want application/json", got)
	}
	// Header names are case-insensitive (RFC 9110) — bash's raw socket read
	// preserves whatever case the sender used, and the gateway is not the
	// only thing that might send this.
	if got := headerValue(headers, "content-type"); got != "application/json" {
		t.Errorf("headerValue is not case-insensitive: got %q", got)
	}
	if got := headerValue(headers, "Missing-Header"); got != "" {
		t.Errorf("headerValue for an absent header = %q, want empty", got)
	}
}

func TestParseHTTPResponseExtractsContentType(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"

	res, ok := parseHTTPResponse(raw)
	if !ok {
		t.Fatal("parseHTTPResponse returned ok=false")
	}
	if res.Status != 200 {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if res.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", res.ContentType)
	}
	if res.Body != "{}" {
		t.Errorf("Body = %q, want {}", res.Body)
	}
}

func TestParseHTTPResponseNoContentType(t *testing.T) {
	raw := "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 3\r\n\r\nfoo"

	res, ok := parseHTTPResponse(raw)
	if !ok {
		t.Fatal("parseHTTPResponse returned ok=false")
	}
	if res.ContentType != "" {
		t.Errorf("ContentType = %q, want empty (no Content-Type header was present)", res.ContentType)
	}
}

// A response with no blank-line terminator at all — truncated mid-headers,
// or otherwise malformed — must fail to parse rather than have the whole
// remainder silently treated as both the header block and the body. This
// is the exact bug an earlier version of parseHTTPResponse had.
func TestParseHTTPResponseNoTerminatorFails(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"headers only, connection cut before the blank line", "HTTP/1.1 200 OK\r\nContent-Type: application/json"},
		{"no headers or body at all, just the status line", "HTTP/1.1 200 OK\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseHTTPResponse(tt.raw); ok {
				t.Errorf("parseHTTPResponse(%q) returned ok=true, want false (no header/body terminator present)", tt.raw)
			}
		})
	}
}

func TestParseHTTPResponseLFOnly(t *testing.T) {
	// The bash probe emits real CRLF from the raw socket, but nothing about
	// parseHTTPResponse should require it — a bare-LF response should parse
	// the same way.
	raw := "HTTP/1.1 200 OK\nContent-Type: text/event-stream\n\ndata: hi\n"

	res, ok := parseHTTPResponse(raw)
	if !ok {
		t.Fatal("parseHTTPResponse returned ok=false")
	}
	if res.ContentType != "text/event-stream" {
		t.Errorf("ContentType = %q, want text/event-stream", res.ContentType)
	}
}
