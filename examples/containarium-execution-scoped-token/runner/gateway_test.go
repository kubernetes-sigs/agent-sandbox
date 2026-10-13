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

func TestGatewayAccepted(t *testing.T) {
	tests := []struct {
		name string
		ct   string
		want bool
	}{
		{"application/json", "application/json", true},
		{"application/json with charset", "application/json; charset=utf-8", true},
		{"text/event-stream", "text/event-stream", true},
		{"what net/http.Error always sets", "text/plain; charset=utf-8", false},
		{"empty (header missing entirely)", "", false},
		{"html error/block page", "text/html; charset=utf-8", false},
		{"looks like json but isn't the real type", "application/json5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gatewayAccepted(httpResult{ContentType: tt.ct})
			if got != tt.want {
				t.Errorf("gatewayAccepted(ContentType=%q) = %v, want %v", tt.ct, got, tt.want)
			}
		})
	}
}

func TestGatewayRefusal(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no known refusal in body", `{"type":"error","error":{"message":"invalid x-api-key"}}`, ""},
		{"revoked", "gateway token revoked", "gateway token revoked"},
		{"missing token", "missing gateway token\n", "missing gateway token"},
		{"substring match inside a longer message", "refused: invalid gateway token (expired)", "invalid gateway token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gatewayRefusal(httpResult{Body: tt.body})
			if got != tt.want {
				t.Errorf("gatewayRefusal(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// This is the exact bug the fix closes. The gateway's ErrorHandler produces
// "upstream error: ..." (via net/http.Error, so Content-Type is forced to
// text/plain) when the reverse proxy itself cannot reach the provider — a
// real gateway-generated refusal that gatewayRejections has never listed.
// Before this fix, gatewayRefusal would return "" for it and it would read
// as accepted; after, gatewayAccepted must reject it on Content-Type alone,
// with no dependence on the refusal-string list at all.
func TestGatewayAcceptedRejectsUnrecognizedGatewayRefusal(t *testing.T) {
	r := httpResult{
		Status:      502,
		Body:        "upstream error: dial tcp: lookup api.anthropic.com: no such host",
		ContentType: "text/plain; charset=utf-8",
	}
	if gatewayAccepted(r) {
		t.Error("gatewayAccepted returned true for a gateway-generated refusal not on gatewayRejections — the accepts-by-default bug is back")
	}
	if refusal := gatewayRefusal(r); refusal != "" {
		t.Fatalf("test setup is wrong: gatewayRefusal unexpectedly matched %q in a body that should not contain any listed refusal", refusal)
	}
}
