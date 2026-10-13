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

// Tests mintGatewayToken directly against the wire format it produces —
// decoding the header and payload it signs, and recomputing the HMAC by
// hand — rather than through a round-trip with a verifier, because this
// package has no verifier of its own (that lives in the gateway, a separate
// binary). Plain `testing`, no assertion library: consistent with
// token.go's own "standard library only" choice, documented there.
//
// Not covered: mintGatewayToken's two error returns (crypto/rand.Read and
// json.Marshal failing). Both call the global stdlib functions directly
// rather than through an injectable seam, and forcing either to fail in a
// unit test would mean adding that seam — a larger change than "add the
// missing tests" asks for. Noted here rather than left silent.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

var jtiPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// b64 is the JWT-standard base64 encoding: unpadded, URL-safe — the same
// encoding mintGatewayToken uses.
var b64 = base64.RawURLEncoding

func TestMintGatewayTokenShape(t *testing.T) {
	secret := []byte("test-secret")
	ttl := 30 * time.Minute
	before := time.Now()
	tok, err := mintGatewayToken(secret, "tenant-a", "skill-b", "run-123", ttl)
	after := time.Now()
	if err != nil {
		t.Fatalf("mintGatewayToken: %v", err)
	}

	if parts := strings.Split(tok.Token, "."); len(parts) != 3 {
		t.Fatalf("token has %d dot-separated parts, want 3 (header.payload.signature): %q", len(parts), tok.Token)
	}

	if !jtiPattern.MatchString(tok.JTI) {
		t.Errorf("JTI = %q, want 32 lowercase hex characters (hex of a 16-byte random value)", tok.JTI)
	}

	if tok.RunID != "run-123" {
		t.Errorf("RunID = %q, want %q", tok.RunID, "run-123")
	}

	// mintGatewayToken computes now := time.Now().Truncate(time.Second)
	// internally, sometime between `before` and `after` above, then adds
	// ttl. Truncation only ever moves `now` earlier, never later, so a
	// couple of seconds of slack on both ends keeps this from flaking
	// without hiding a real bug (wrong unit, ttl ignored, etc.).
	wantMin := before.Add(ttl).Add(-2 * time.Second)
	wantMax := after.Add(ttl).Add(time.Second)
	if tok.ExpiresAt.Before(wantMin) || tok.ExpiresAt.After(wantMax) {
		t.Errorf("ExpiresAt = %v, want within [%v, %v] (now + ttl)", tok.ExpiresAt, wantMin, wantMax)
	}
}

func TestMintGatewayTokenClaimsAndSignature(t *testing.T) {
	secret := []byte("shared-secret")
	tok, err := mintGatewayToken(secret, "tenant-a", "skill-b", "run-123", time.Hour)
	if err != nil {
		t.Fatalf("mintGatewayToken: %v", err)
	}

	parts := strings.Split(tok.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	headerBytes, err := b64.DecodeString(headerB64)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["alg"] != "HS256" {
		t.Errorf("header alg = %q, want HS256", header["alg"])
	}
	if header["typ"] != "JWT" {
		t.Errorf("header typ = %q, want JWT", header["typ"])
	}

	payloadBytes, err := b64.DecodeString(payloadB64)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims gatewayClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Tenant != "tenant-a" {
		t.Errorf("Tenant = %q, want tenant-a", claims.Tenant)
	}
	if claims.SkillID != "skill-b" {
		t.Errorf("SkillID = %q, want skill-b", claims.SkillID)
	}
	if claims.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", claims.Provider)
	}
	if claims.RunID != "run-123" {
		t.Errorf("claims.RunID = %q, want run-123", claims.RunID)
	}
	if claims.Issuer != gatewayIssuer {
		t.Errorf("Issuer = %q, want %q — a token minted without this issuer must not be usable at the gateway", claims.Issuer, gatewayIssuer)
	}
	if claims.Subject != "tenant-a" {
		t.Errorf("Subject = %q, want tenant-a", claims.Subject)
	}
	if claims.ID != tok.JTI {
		t.Errorf("claims.jti = %q, want it to match the returned JTI %q — the minter revokes by this value alone", claims.ID, tok.JTI)
	}
	if claims.IssuedAt == 0 {
		t.Error("IssuedAt is zero")
	}
	if claims.ExpiresAt != tok.ExpiresAt.Unix() {
		t.Errorf("claims.exp = %d, want %d (tok.ExpiresAt.Unix())", claims.ExpiresAt, tok.ExpiresAt.Unix())
	}

	// The signature has to be exactly HMAC-SHA256 over "header.payload"
	// under the SAME secret the gateway is configured with — this, not the
	// token's shape, is what actually lets the gateway trust a token it
	// never minted itself.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerB64 + "." + payloadB64))
	wantSig := b64.EncodeToString(mac.Sum(nil))
	if sigB64 != wantSig {
		t.Errorf("signature = %q, want %q (HMAC-SHA256 of header.payload under the minting secret)", sigB64, wantSig)
	}

	// And it must NOT verify under a different secret — otherwise the
	// shared-secret trust model this whole example rests on is broken.
	wrongMAC := hmac.New(sha256.New, []byte("a-different-secret"))
	wrongMAC.Write([]byte(headerB64 + "." + payloadB64))
	if sigB64 == b64.EncodeToString(wrongMAC.Sum(nil)) {
		t.Error("signature matched under a different secret — HMAC is not actually keyed by the minting secret")
	}
}

func TestMintGatewayTokenUniqueJTIPerCall(t *testing.T) {
	secret := []byte("secret")
	a, err := mintGatewayToken(secret, "tenant", "skill", "run-1", time.Minute)
	if err != nil {
		t.Fatalf("mintGatewayToken (1st): %v", err)
	}
	b, err := mintGatewayToken(secret, "tenant", "skill", "run-1", time.Minute)
	if err != nil {
		t.Fatalf("mintGatewayToken (2nd): %v", err)
	}

	// Same arguments, but revocation ("kill exactly what I issued, nothing
	// else") only holds if every mint gets an independently revocable jti —
	// this is the property the whole example's check 4 depends on.
	if a.JTI == b.JTI {
		t.Errorf("two calls with identical arguments produced the same jti %q", a.JTI)
	}
	if a.Token == b.Token {
		t.Error("two calls with identical arguments produced the identical token")
	}
}

func TestMintGatewayTokenOmitsEmptyOptionalClaims(t *testing.T) {
	tests := []struct {
		name        string
		skillID     string
		runID       string
		wantSkillID bool
		wantRunID   bool
	}{
		{name: "both set", skillID: "skill-b", runID: "run-123", wantSkillID: true, wantRunID: true},
		{name: "both empty", skillID: "", runID: "", wantSkillID: false, wantRunID: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := mintGatewayToken([]byte("secret"), "tenant", tt.skillID, tt.runID, time.Minute)
			if err != nil {
				t.Fatalf("mintGatewayToken: %v", err)
			}
			parts := strings.Split(tok.Token, ".")
			payloadBytes, err := b64.DecodeString(parts[1])
			if err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(payloadBytes, &raw); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if _, has := raw["skill_id"]; has != tt.wantSkillID {
				t.Errorf("skill_id present = %v, want %v (raw claims: %v)", has, tt.wantSkillID, raw)
			}
			if _, has := raw["run_id"]; has != tt.wantRunID {
				t.Errorf("run_id present = %v, want %v (raw claims: %v)", has, tt.wantRunID, raw)
			}
		})
	}
}
