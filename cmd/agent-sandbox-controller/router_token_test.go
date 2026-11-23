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

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "mint-router-token" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMintRouterToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	discovery := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/apis/agents.x-k8s.io/v1beta1/namespaces/team-a/sandboxes/box":
			fmt.Fprint(w, `{"apiVersion":"agents.x-k8s.io/v1beta1","kind":"Sandbox","metadata":{"namespace":"team-a","name":"box","uid":"sandbox-uid"}}`)
		case req.Method == http.MethodGet && req.URL.Path == "/apis/agents.x-k8s.io/v1beta1/namespaces/team-b/sandboxes/deleting":
			fmt.Fprint(w, `{"apiVersion":"agents.x-k8s.io/v1beta1","kind":"Sandbox","metadata":{"namespace":"team-b","name":"deleting","uid":"deleting-uid","deletionTimestamp":"2026-01-01T00:00:00Z"}}`)
		case req.URL.Path == "/apis" || req.URL.Path == "/api":
			select {
			case discovery <- req.URL.Path:
			default:
			}
			// Hold discovery open so the test can observe which path the command took.
			<-ctx.Done()
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
		}
	}))
	t.Cleanup(api.Close)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    namespace: team-a
current-context: test
`, api.URL), 0600))
	t.Setenv("KUBECONFIG", configPath)

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	keyPath := filepath.Join(dir, "signing-key.pem")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))

	cmd := exec.CommandContext(ctx, os.Args[0], "mint-router-token",
		"--signing-key-file", keyPath, "--key-id", "current", "box")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case path := <-discovery:
		cancel()
		waitErr := <-done
		t.Fatalf("controller started API discovery at %s instead of issuing a scoped v2 token for an existing Sandbox (command exit: %v)", path, waitErr)
	case err := <-done:
		require.NoError(t, err, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	require.True(t, strings.HasPrefix(token, "v2.current."), "controller did not issue a scoped v2 token for an existing Sandbox")
	require.Equal(t, token+"\n", stdout.String())

	authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": publicKey},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/v1/files/report.txt", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	target := authz.AuthorizationTarget{
		Namespace: "team-a", SandboxName: "box", SandboxUID: "sandbox-uid",
		Port: 8080, Method: req.Method, Path: req.URL.Path,
	}
	require.NoError(t, authorizer.Authorize(t.Context(), req, target))
	target.SandboxUID = "replacement-uid"
	require.ErrorIs(t, authorizer.Authorize(t.Context(), req, target), authz.ErrForbidden)

	for _, tc := range []struct {
		name string
		args []string
		err  string
	}{
		{name: "missing sandbox", args: []string{"missing"}, err: "get Sandbox team-a/missing"},
		{name: "deleting sandbox in explicit namespace", args: []string{"--namespace", "team-b", "deleting"}, err: "sandbox team-b/deleting is being deleted"},
		{name: "subsecond lifetime", args: []string{"--ttl", "500ms", "box"}, err: "--ttl must be at least 1s"},
		{name: "excessive lifetime", args: []string{"--ttl", "8760h", "box"}, err: "--ttl must be at most 1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"mint-router-token", "--signing-key-file", keyPath, "--key-id", "current"}, tc.args...)
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			require.Error(t, err)
			require.Empty(t, output)
			require.Contains(t, stderr.String(), tc.err)
		})
	}
}
