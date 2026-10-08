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
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	sandboxd "sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/proxy"
)

func TestExampleThroughRouter(t *testing.T) {
	daemon, err := sandboxd.New(sandboxd.Options{RootDir: t.TempDir(), Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := grpc.NewServer()
	daemon.RegisterGRPC(backend)
	go func() { _ = backend.Serve(ln) }()
	t.Cleanup(func() { backend.Stop(); daemon.ShutdownProcesses(context.Background()) })
	host, rawPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte("a"), authz.MinScopedTokenSecretLen)
	authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "box-a", Namespace: "default", Labels: map[string]string{cache.PodSandboxNameHashLabel: "demo"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: "box-a", UID: "uid-a", Controller: new(true)}},
		},
		Status: corev1.PodStatus{PodIP: host, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	lookup, err := cache.New(cache.Options{Client: fake.NewClientset(pod), Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	cacheCtx, cacheCancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cacheCancel)
	lookup.Start(cacheCtx)
	if !lookup.WaitForSync(cacheCtx) {
		t.Fatal("discovery cache failed to sync")
	}
	h := proxy.NewHandler(proxy.Options{Config: &cfg, Authorizer: authorizer, Cache: lookup})
	t.Cleanup(h.CloseIdleConnections)
	router := httptest.NewUnstartedServer(h)
	router.Config.Protocols = new(http.Protocols)
	router.Config.Protocols.SetUnencryptedHTTP2(true)
	router.EnableHTTP2 = true
	router.StartTLS()
	t.Cleanup(router.Close)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: router.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, secret, 0600); err != nil {
		t.Fatal(err)
	}
	var token bytes.Buffer
	_, err = run(t.Context(), options{Mode: "mint-token", Namespace: "default", Sandbox: "box-a", SecretFile: secretPath, TokenTTL: time.Minute}, nil, &token, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, token.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("token output error context", func(t *testing.T) {
		sinkErr := errors.New("output closed")
		_, err := run(t.Context(), options{Mode: "mint-token", Namespace: "default", Sandbox: "box-a", SecretFile: secretPath, TokenTTL: time.Minute}, nil, outputErrorWriter{sinkErr}, io.Discard)
		if !errors.Is(err, sinkErr) || !strings.HasPrefix(err.Error(), "write scoped token:") {
			t.Fatalf("token output error lost context or cause: %v", err)
		}
	})
	opts := options{Address: strings.TrimPrefix(router.URL, "https://"), CAFile: caPath, Timeout: 3 * time.Second, Namespace: "default", Sandbox: "box-a", Port: port, TokenFile: tokenPath}
	for _, mode := range []string{"execute", "start", "interact", "signal"} {
		t.Run(mode, func(t *testing.T) {
			opts.Mode = mode
			opts.Input = "through-router"
			var stdout, stderr bytes.Buffer
			args := []string{"/bin/sh", "-c", "printf hello; printf warning >&2; exit 7"}
			if mode == "interact" || mode == "signal" {
				args = nil
			}
			code, err := run(t.Context(), opts, args, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "execute", "start":
				if code != 7 || stdout.String() != "hello" || !strings.Contains(stderr.String(), "warning") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
				}
			case "interact":
				if code != 0 || !strings.Contains(stdout.String(), "input:through-router") {
					t.Fatalf("code=%d stdout=%q", code, &stdout)
				}
			case "signal":
				if code == 0 || !strings.Contains(stdout.String(), "ready") {
					t.Fatalf("code=%d stdout=%q", code, &stdout)
				}
			}
		})
	}
	for _, tc := range []struct {
		mode, context string
		stdout        bool
	}{
		{"execute", "write command stdout:", true},
		{"execute", "write command stderr:", false},
		{"start", "write process ID:", false},
		{"start", "write process stdout:", true},
	} {
		t.Run(tc.context, func(t *testing.T) {
			sinkErr := errors.New("output closed")
			var stdout, stderr = io.Discard, io.Discard
			if tc.stdout {
				stdout = outputErrorWriter{sinkErr}
			} else {
				stderr = outputErrorWriter{sinkErr}
			}
			opts.Mode = tc.mode
			_, err := run(t.Context(), opts, []string{"/bin/sh", "-c", "printf hello; printf warning >&2; exit 7"}, stdout, stderr)
			if !errors.Is(err, sinkErr) || !strings.HasPrefix(err.Error(), tc.context) {
				t.Fatalf("output error lost context or cause: %v", err)
			}
		})
	}
	t.Run("wrong TLS server name", func(t *testing.T) {
		opts.Mode = "execute"
		opts.ServerName = "not-the-router.invalid"
		opts.Timeout = 300 * time.Millisecond
		if _, err := run(t.Context(), opts, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("unverified server name connected")
		}
	})
	t.Run("missing CA trust", func(t *testing.T) {
		opts.Mode = "execute"
		opts.ServerName = ""
		opts.CAFile = ""
		opts.Timeout = 300 * time.Millisecond
		if _, err := run(t.Context(), opts, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("untrusted certificate connected")
		}
	})
}

func TestExampleRequiresFiniteBudget(t *testing.T) {
	if _, err := run(t.Context(), options{Mode: "execute", Address: "127.0.0.1:1", Plaintext: true}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("example accepted an unlimited RPC")
	}
}

type outputErrorWriter struct{ err error }

func (w outputErrorWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 0, w.err
}
