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
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	scopedTokenV2Version        = "v2"
	scopedTokenV2SigningContext = "agent-sandbox/scoped-token/v2."
	maxScopedTokenKeyIDLen      = 128
)

type scopedTokenV2Claims struct {
	Namespace   string `json:"ns"`
	SandboxName string `json:"name"`
	SandboxUID  string `json:"uid"`
	Port        int    `json:"port"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Exp         int64  `json:"exp"`
}

type scopedTokenVerificationKeyFile struct {
	Keys []scopedTokenVerificationKey `json:"keys"`
}

type scopedTokenVerificationKey struct {
	KeyID     string `json:"kid"`
	PublicKey string `json:"publicKey"`
}

// ParseScopedTokenVerificationKeys parses a JSON public-key set for the
// router. Each publicKey is an unpadded base64url Ed25519 public key.
func ParseScopedTokenVerificationKeys(data []byte) (map[string]ed25519.PublicKey, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var keyFile scopedTokenVerificationKeyFile
	if err := dec.Decode(&keyFile); err != nil {
		return nil, fmt.Errorf("scopedtoken: decode verification keys: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("scopedtoken: verification key file contains trailing data")
		}
		return nil, fmt.Errorf("scopedtoken: decode verification key trailer: %w", err)
	}
	if len(keyFile.Keys) == 0 {
		return nil, errors.New("scopedtoken: verification key file must contain at least one key")
	}

	keys := make(map[string]ed25519.PublicKey, len(keyFile.Keys))
	for _, entry := range keyFile.Keys {
		if !validScopedTokenKeyID(entry.KeyID) {
			return nil, fmt.Errorf("scopedtoken: invalid key ID %q", entry.KeyID)
		}
		if _, exists := keys[entry.KeyID]; exists {
			return nil, fmt.Errorf("scopedtoken: duplicate key ID %q", entry.KeyID)
		}
		key, err := base64.RawURLEncoding.DecodeString(entry.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("scopedtoken: decode public key %q: %w", entry.KeyID, err)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("scopedtoken: public key %q must be %d bytes, got %d", entry.KeyID, ed25519.PublicKeySize, len(key))
		}
		keys[entry.KeyID] = append(ed25519.PublicKey(nil), key...)
	}
	return keys, nil
}

// MintScopedTokenV2 produces an Ed25519 token bound to one canonical
// authorization target. The key ID is protected by the signature so it
// cannot be swapped to select a different verification key.
func MintScopedTokenV2(privateKey ed25519.PrivateKey, keyID string, target AuthorizationTarget, ttl time.Duration) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("scopedtoken: private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(privateKey))
	}
	if !validScopedTokenKeyID(keyID) {
		return "", fmt.Errorf("scopedtoken: invalid key ID %q", keyID)
	}
	if target.SandboxUID == "" {
		return "", errors.New("scopedtoken: Sandbox UID is required")
	}
	normalized, err := NormalizeAuthorizationTarget(target)
	if err != nil {
		return "", fmt.Errorf("scopedtoken: %w", err)
	}
	if ttl < time.Second {
		return "", fmt.Errorf("scopedtoken: ttl must be at least %s (exp has one-second resolution), got %s", time.Second, ttl)
	}
	claims := scopedTokenV2Claims{
		Namespace:   normalized.Namespace,
		SandboxName: normalized.SandboxName,
		SandboxUID:  normalized.SandboxUID,
		Port:        normalized.Port,
		Method:      normalized.Method,
		Path:        normalized.Path,
		Exp:         time.Now().Add(ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("scopedtoken: marshal v2 claims: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(privateKey, scopedTokenV2SigningInput(keyID, encPayload))
	return strings.Join([]string{
		scopedTokenV2Version,
		keyID,
		encPayload,
		base64.RawURLEncoding.EncodeToString(signature),
	}, "."), nil
}

func scopedTokenV2SigningInput(keyID, encPayload string) []byte {
	return []byte(scopedTokenV2SigningContext + keyID + "." + encPayload)
}

func validScopedTokenKeyID(keyID string) bool {
	if keyID == "" || len(keyID) > maxScopedTokenKeyIDLen {
		return false
	}
	for i := range len(keyID) {
		c := keyID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
