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

package authz

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func leaf(uris []string, dns []string, cn string, org []string) *x509.Certificate {
	parsedURIs := make([]*url.URL, 0, len(uris))
	for _, u := range uris {
		pu, _ := url.Parse(u)
		parsedURIs = append(parsedURIs, pu)
	}
	return &x509.Certificate{
		URIs:     parsedURIs,
		DNSNames: dns,
		Subject:  pkix.Name{CommonName: cn, Organization: org},
	}
}

func stateWith(c *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{c}}}
}

func TestIdentityFromTLS(t *testing.T) {
	cases := []struct {
		name  string
		state *tls.ConnectionState
		want  Identity
	}{
		{
			name:  "nil state",
			state: nil,
			want:  Identity{},
		},
		{
			name:  "no verified chain",
			state: &tls.ConnectionState{},
			want:  Identity{},
		},
		{
			name:  "spiffe URI wins over DNS and CN",
			state: stateWith(leaf([]string{"spiffe://example.com/ns/team/sa/agent"}, []string{"agent.team.svc"}, "agent", nil)),
			want:  Identity{Source: "tls", Username: "spiffe://example.com/ns/team/sa/agent"},
		},
		{
			name:  "DNS SAN beats CN when no SPIFFE",
			state: stateWith(leaf(nil, []string{"agent.team.svc.cluster.local"}, "agent-cn", nil)),
			want:  Identity{Source: "tls", Username: "agent.team.svc.cluster.local"},
		},
		{
			name:  "CN fallback",
			state: stateWith(leaf(nil, nil, "cn-only", nil)),
			want:  Identity{Source: "tls", Username: "cn-only"},
		},
		{
			name:  "Organization becomes groups",
			state: stateWith(leaf(nil, nil, "u", []string{"system:masters", "ops"})),
			want:  Identity{Source: "tls", Username: "u", Groups: []string{"system:masters", "ops"}},
		},
		{
			name:  "non-spiffe URI is ignored, falls back to DNS",
			state: stateWith(leaf([]string{"https://example.com/ignored"}, []string{"agent.svc"}, "cn", nil)),
			want:  Identity{Source: "tls", Username: "agent.svc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IdentityFromTLS(tc.state)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestBearerTokenFromRequest(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{"missing header", "", "", false},
		{"basic auth ignored", "Basic dXNlcjpwYXNz", "", false},
		{"happy path", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"case insensitive scheme", "bearer abc", "abc", true},
		{"BEARER scheme", "BEARER xyz", "xyz", true},
		{"trims whitespace around token", "Bearer    spaced.token   ", "spaced.token", true},
		{"empty token rejected", "Bearer ", "", false},
		{"empty token after trim rejected", "Bearer    ", "", false},
		{"shorter than prefix", "Bear", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set(AuthorizationHeader, tc.header)
			}
			tok, ok := BearerTokenFromRequest(req)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v (token=%q)", ok, tc.wantOK, tok)
			}
			if tok != tc.wantToken {
				t.Fatalf("token: got %q want %q", tok, tc.wantToken)
			}
		})
	}
}

func TestBearerTokenFromRequestNilRequest(t *testing.T) {
	if _, ok := BearerTokenFromRequest(nil); ok {
		t.Fatal("nil request should not produce a token")
	}
}
