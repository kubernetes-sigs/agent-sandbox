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
	"net/http"
	"strings"
)

// AuthorizationHeader is the HTTP header carrying a Bearer token. The
// constant is exported so the proxy and tests share a single source of
// truth.
const AuthorizationHeader = "Authorization"

// BearerSchemePrefix is the case-insensitive prefix that introduces a
// Bearer token in the Authorization header.
const BearerSchemePrefix = "Bearer "

// IdentityFromTLS extracts an Identity from the peer's verified TLS
// client certificate. Returns the zero Identity (Source=="") when no
// verified cert is available — typically because mTLS is off or
// optional and the client didn't present one.
//
// Name precedence: first SPIFFE URI SAN → first DNS SAN → Subject CN.
// This ordering favors SPIFFE in service-mesh deployments, falls back
// to DNS SANs which are how K8s ServiceAccount certs are typically
// shaped, and uses the CN only when nothing else is available.
func IdentityFromTLS(state *tls.ConnectionState) Identity {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return Identity{}
	}
	leaf := state.VerifiedChains[0][0]
	id := Identity{Source: "tls"}

	for _, u := range leaf.URIs {
		if strings.EqualFold(u.Scheme, "spiffe") {
			id.Username = u.String()
			break
		}
	}
	if id.Username == "" && len(leaf.DNSNames) > 0 {
		id.Username = leaf.DNSNames[0]
	}
	if id.Username == "" && leaf.Subject.CommonName != "" {
		id.Username = leaf.Subject.CommonName
	}
	// O groups become group claims, which mirrors how K8s shapes
	// client-cert identities (group = O, user = CN).
	if len(leaf.Subject.Organization) > 0 {
		id.Groups = append(id.Groups, leaf.Subject.Organization...)
	}
	return id
}

// BearerTokenFromRequest extracts a Bearer token from the Authorization
// header. Returns ("", false) when the header is missing or does not
// start with the case-insensitive "Bearer " prefix.
//
// The scheme match is case-insensitive per RFC 7235 §2.1 ("scheme
// names are matched case-insensitively") but the token itself is
// returned verbatim — tokens are case-sensitive.
func BearerTokenFromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	h := r.Header.Get(AuthorizationHeader)
	if len(h) < len(BearerSchemePrefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(BearerSchemePrefix)], BearerSchemePrefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(BearerSchemePrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
