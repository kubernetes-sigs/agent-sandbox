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

package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// healthPath is served without a token so the kubelet's readiness probe,
// which carries no per-sandbox credential, keeps working. Every other path,
// /v1/metadata included, requires the token.
const healthPath = "/v1/health"

// AuthTokenDigestSize is the length in bytes of the token digest
// Options.AuthTokenSHA256 holds.
const AuthTokenDigestSize = sha256.Size

// ParseAuthTokenSHA256 decodes the hex SHA-256 digest of the bearer token
// (64 hex characters). Configuring the digest rather than the token keeps the
// token itself out of the pod spec, the process environment and anything
// copied from them.
func ParseAuthTokenSHA256(hexDigest string) ([]byte, error) {
	digest, err := hex.DecodeString(strings.TrimSpace(hexDigest))
	if err != nil {
		return nil, fmt.Errorf("auth token sha256: %w", err)
	}
	if len(digest) != AuthTokenDigestSize {
		return nil, fmt.Errorf("auth token sha256: want %d bytes, got %d", AuthTokenDigestSize, len(digest))
	}
	return digest, nil
}

// LoadAuthTokenFile reads the bearer token from path and returns its SHA-256
// digest. Surrounding whitespace (e.g. a trailing newline) is ignored; an
// empty token is an error, so a misconfigured mount never silently disables
// authentication.
func LoadAuthTokenFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("auth token file is empty")
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], nil
}

// tokenChecker validates "Authorization: Bearer <token>" values against the
// configured token's SHA-256 digest in constant time. Only the digest is
// held, so the daemon never needs the token itself.
type tokenChecker struct {
	want [sha256.Size]byte
}

func newTokenChecker(digest []byte) *tokenChecker {
	c := &tokenChecker{}
	copy(c.want[:], digest)
	return c
}

func (c *tokenChecker) valid(header string) bool {
	scheme, credential, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(credential)))
	return subtle.ConstantTimeCompare(got[:], c.want[:]) == 1
}

func (c *tokenChecker) authorizeGRPC(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if slices.ContainsFunc(md.Get("authorization"), c.valid) {
		return nil
	}
	return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
}

func (c *tokenChecker) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (any, error) {
	if err := c.authorizeGRPC(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (c *tokenChecker) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo,
	handler grpc.StreamHandler) error {
	if err := c.authorizeGRPC(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

// middleware rejects REST requests without the token, except the health
// probe.
func (c *tokenChecker) middleware(next http.Handler, s *RESTServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath && !c.valid(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandboxd"`)
			s.writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED",
				"missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
