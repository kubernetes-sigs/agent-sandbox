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

package cache_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	sandboxcache "sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/proxy"
)

const (
	conformanceNamespace = "team-a"
	conformanceName      = "sandbox-7"
	conformanceOldUID    = types.UID("11111111-1111-4111-8111-111111111111")
	conformanceNewUID    = types.UID("22222222-2222-4222-8222-222222222222")
	conformanceV1Secret  = "0123456789abcdef0123456789abcdef"
)

type conformanceFixture struct {
	t          *testing.T
	client     *fake.Clientset
	cache      *sandboxcache.Cache
	backend    *httptest.Server
	router     *httptest.Server
	port       int
	oldPrivate ed25519.PrivateKey
	newPrivate ed25519.PrivateKey
}

func newConformanceFixture(t *testing.T, pod *corev1.Pod) *conformanceFixture {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	port, err := strconv.Atoi(backendURL.Port())
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	if pod != nil {
		pod.Status.PodIP = backendURL.Hostname()
	}

	var objects []runtime.Object
	if pod != nil {
		objects = append(objects, pod)
	}
	client := fake.NewSimpleClientset(objects...)
	cache, err := sandboxcache.New(sandboxcache.Options{
		Client: client,
		Log:    logr.Discard(),
		Resync: time.Hour,
	})
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cache.Start(ctx)
	if !cache.WaitForSync(ctx) {
		t.Fatal("cache did not sync")
	}

	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate old Ed25519 key: %v", err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate new Ed25519 key: %v", err)
	}
	authorizer, err := authz.NewScopedTokenAuthorizer(authz.ScopedTokenOptions{
		Secret: []byte(conformanceV1Secret),
		VerificationKeys: map[string]ed25519.PublicKey{
			"old": oldPublic,
			"new": newPublic,
		},
		V1AcceptUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("new scoped-token authorizer: %v", err)
	}

	cfg := config.Defaults()
	cfg.PathRoutingPrefix = "/router"
	cfg.ProxyTimeout = 2 * time.Second
	cfg.UpstreamMaxRetries = 0
	router := httptest.NewServer(proxy.NewHandler(proxy.Options{
		Config:     &cfg,
		Cache:      cache,
		Authorizer: authorizer,
		Logger:     logr.Discard(),
	}))
	t.Cleanup(router.Close)

	return &conformanceFixture{
		t:          t,
		client:     client,
		cache:      cache,
		backend:    backend,
		router:     router,
		port:       port,
		oldPrivate: oldPrivate,
		newPrivate: newPrivate,
	}
}

func conformancePod(uid types.UID, unclaimed bool) *corev1.Pod {
	controller := true
	labels := map[string]string{sandboxcache.PodSandboxNameHashLabel: "name-hash"}
	if unclaimed {
		labels[sandboxcache.PodWarmPoolLabel] = "pool-hash"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      conformanceName,
			Namespace: conformanceNamespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxcache.SandboxAPIGroup + "/v1beta1",
				Kind:       sandboxcache.SandboxKind,
				Name:       conformanceName,
				UID:        uid,
				Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{
			PodIP: "127.0.0.1",
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func (f *conformanceFixture) waitForResolution(uid types.UID) {
	f.t.Helper()
	waitForConformance(f.t, func() bool {
		e, _, ok := f.cache.Resolve(conformanceNamespace, conformanceName, uid)
		return ok && e.SandboxUID == uid
	})
}

func waitForConformance(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func (f *conformanceFixture) token(privateKey ed25519.PrivateKey, keyID string, uid types.UID) string {
	f.t.Helper()
	token, err := authz.MintScopedTokenV2(privateKey, keyID, authz.AuthorizationTarget{
		Namespace:   conformanceNamespace,
		SandboxName: conformanceName,
		SandboxUID:  string(uid),
		Port:        f.port,
		Method:      http.MethodGet,
		Path:        "/run",
	}, time.Minute)
	if err != nil {
		f.t.Fatalf("mint scoped token: %v", err)
	}
	return token
}

func (f *conformanceFixture) request(token, uid, method, path string, port int, pathRouted bool) int {
	f.t.Helper()
	requestURL := f.router.URL + path
	if pathRouted {
		requestURL = fmt.Sprintf("%s/router/%s/%s/%d%s", f.router.URL, conformanceNamespace, conformanceName, port, path)
	}
	req, err := http.NewRequest(method, requestURL, nil)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	if !pathRouted {
		req.Header.Set(proxy.HeaderSandboxID, conformanceName)
		req.Header.Set(proxy.HeaderSandboxNamespace, conformanceNamespace)
		req.Header.Set(proxy.HeaderSandboxPort, strconv.Itoa(port))
		if uid != "" {
			req.Header.Set(proxy.HeaderSandboxUID, uid)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestScopedTokenV2CacheConformance_UnclaimedMemberAndAdoption(t *testing.T) {
	pod := conformancePod(conformanceOldUID, true)
	f := newConformanceFixture(t, pod)
	f.waitForResolution(conformanceOldUID)
	token := f.token(f.oldPrivate, "old", conformanceOldUID)

	if status := f.request(token, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusNoContent {
		t.Fatalf("exact UID must reach an unclaimed warm member: got %d want %d", status, http.StatusNoContent)
	}
	if status := f.request(token, "", http.MethodGet, "/run", f.port, true); status != http.StatusForbidden {
		t.Fatalf("unclaimed warm member must not resolve by name: got %d want %d", status, http.StatusForbidden)
	}

	adopted := pod.DeepCopy()
	delete(adopted.Labels, sandboxcache.PodWarmPoolLabel)
	if _, err := f.client.CoreV1().Pods(conformanceNamespace).Update(t.Context(), adopted, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("adopt warm member: %v", err)
	}
	waitForConformance(t, func() bool {
		e, source, ok := f.cache.Resolve(conformanceNamespace, conformanceName, "")
		return ok && source == sandboxcache.ResolutionByName && e.SandboxUID == conformanceOldUID
	})
	if status := f.request(token, "", http.MethodGet, "/run", f.port, true); status != http.StatusNoContent {
		t.Fatalf("adopted member must resolve by canonical name: got %d want %d", status, http.StatusNoContent)
	}
	legacyToken, err := authz.MintScopedToken([]byte(conformanceV1Secret), conformanceNamespace, conformanceName, time.Minute)
	if err != nil {
		t.Fatalf("mint legacy token: %v", err)
	}
	if status := f.request(legacyToken, "", http.MethodGet, "/run", f.port, true); status != http.StatusNoContent {
		t.Fatalf("v1 token must keep name routing during bounded drain: got %d want %d", status, http.StatusNoContent)
	}
}

func TestScopedTokenV2CacheConformance_ReplacementEventOrders(t *testing.T) {
	t.Run("new add before old delete", func(t *testing.T) {
		oldPod := conformancePod(conformanceOldUID, false)
		f := newConformanceFixture(t, oldPod)
		f.waitForResolution(conformanceOldUID)
		oldToken := f.token(f.oldPrivate, "old", conformanceOldUID)
		newToken := f.token(f.newPrivate, "new", conformanceNewUID)

		replacement := oldPod.DeepCopy()
		replacement.OwnerReferences[0].UID = conformanceNewUID
		if _, err := f.client.CoreV1().Pods(conformanceNamespace).Update(t.Context(), replacement, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("publish replacement before old delete: %v", err)
		}
		waitForConformance(t, func() bool {
			e, source, ok := f.cache.Resolve(conformanceNamespace, conformanceName, conformanceOldUID)
			return ok && source == sandboxcache.ResolutionByName && e.SandboxUID == conformanceNewUID
		})
		if _, ok := f.cache.Get(conformanceOldUID); ok {
			t.Fatal("owner-UID update must remove the old cache entry")
		}
		if status := f.request(oldToken, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusForbidden {
			t.Fatalf("stale token must fail after canonical owner changes: got %d want %d", status, http.StatusForbidden)
		}
		if status := f.request(newToken, string(conformanceNewUID), http.MethodGet, "/run", f.port, false); status != http.StatusNoContent {
			t.Fatalf("replacement token must reach current member: got %d want %d", status, http.StatusNoContent)
		}
	})

	t.Run("old delete before new add", func(t *testing.T) {
		oldPod := conformancePod(conformanceOldUID, false)
		f := newConformanceFixture(t, oldPod)
		f.waitForResolution(conformanceOldUID)
		oldToken := f.token(f.oldPrivate, "old", conformanceOldUID)
		newToken := f.token(f.newPrivate, "new", conformanceNewUID)

		if err := f.client.CoreV1().Pods(conformanceNamespace).Delete(t.Context(), conformanceName, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete old member: %v", err)
		}
		waitForConformance(t, func() bool {
			_, _, ok := f.cache.Resolve(conformanceNamespace, conformanceName, conformanceOldUID)
			return !ok
		})
		if status := f.request(oldToken, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusForbidden {
			t.Fatalf("cache miss must reject old token: got %d want %d", status, http.StatusForbidden)
		}

		newPod := conformancePod(conformanceNewUID, false)
		backendURL, err := url.Parse(f.backend.URL)
		if err != nil {
			t.Fatalf("parse backend URL: %v", err)
		}
		newPod.Status.PodIP = backendURL.Hostname()
		if _, err := f.client.CoreV1().Pods(conformanceNamespace).Create(t.Context(), newPod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create replacement: %v", err)
		}
		f.waitForResolution(conformanceNewUID)
		if status := f.request(oldToken, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusForbidden {
			t.Fatalf("stale token must fail after replacement: got %d want %d", status, http.StatusForbidden)
		}
		if status := f.request(newToken, string(conformanceNewUID), http.MethodGet, "/run", f.port, false); status != http.StatusNoContent {
			t.Fatalf("replacement token must pass after add: got %d want %d", status, http.StatusNoContent)
		}
	})
}

func TestScopedTokenV2CacheConformance_ClaimBindingAndKeyOverlap(t *testing.T) {
	pod := conformancePod(conformanceOldUID, false)
	f := newConformanceFixture(t, pod)
	f.waitForResolution(conformanceOldUID)
	oldToken := f.token(f.oldPrivate, "old", conformanceOldUID)
	newToken := f.token(f.newPrivate, "new", conformanceOldUID)

	for keyID, token := range map[string]string{"old": oldToken, "new": newToken} {
		t.Run(keyID+" key", func(t *testing.T) {
			if status := f.request(token, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusNoContent {
				t.Fatalf("overlapping %s key must authorize: got %d want %d", keyID, status, http.StatusNoContent)
			}
		})
	}

	otherPort := f.port + 1
	if otherPort > 65535 {
		otherPort = f.port - 1
	}
	tests := []struct {
		name   string
		method string
		path   string
		port   int
	}{
		{name: "method", method: http.MethodPost, path: "/run", port: f.port},
		{name: "port", method: http.MethodGet, path: "/run", port: otherPort},
		{name: "path", method: http.MethodGet, path: "/other", port: f.port},
	}
	for _, test := range tests {
		t.Run("wrong "+test.name, func(t *testing.T) {
			if status := f.request(oldToken, string(conformanceOldUID), test.method, test.path, test.port, false); status != http.StatusForbidden {
				t.Fatalf("wrong %s must be forbidden: got %d want %d", test.name, status, http.StatusForbidden)
			}
		})
	}

	if err := f.client.CoreV1().Pods(conformanceNamespace).Delete(t.Context(), conformanceName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete member: %v", err)
	}
	waitForConformance(t, func() bool {
		_, _, ok := f.cache.Resolve(conformanceNamespace, conformanceName, conformanceOldUID)
		return !ok
	})
	if status := f.request(oldToken, string(conformanceOldUID), http.MethodGet, "/run", f.port, false); status != http.StatusForbidden {
		t.Fatalf("cache miss must be forbidden: got %d want %d", status, http.StatusForbidden)
	}
}
