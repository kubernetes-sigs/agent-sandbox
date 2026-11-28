//go:build integration

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

package controllers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	sandbox "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type ttlAPIRoundTripper func(*http.Request) (*http.Response, error)

func (f ttlAPIRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type ttlWatchGate struct {
	io.ReadCloser
	context.Context
	*atomic.Bool
	released <-chan struct{}
}

func (b ttlWatchGate) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.Load() {
		select {
		case <-b.released:
		case <-b.Done():
			return 0, b.Err()
		}
	}
	return n, err
}

func TestSandboxClaimPoolTTLIntegration(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("set KUBEBUILDER_ASSETS to a real kube-apiserver and etcd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	useExistingCluster := false
	testEnv := &envtest.Environment{UseExistingCluster: &useExistingCluster, CRDDirectoryPaths: []string{filepath.Join("..", "..", "k8s", "crds")}, ErrorIfCRDPathMissing: true}
	testEnv.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := newScheme(t)
	live, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	setup := func(t *testing.T, poolName, claimName string, defaults *ext.SandboxClaimDefaults) (*ext.SandboxWarmPool, *ext.SandboxClaim) {
		t.Helper()
		pool := &ext.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"}, Spec: ext.SandboxWarmPoolSpec{TemplateRef: ext.SandboxTemplateRef{Name: "ttl-fixture"}, ClaimDefaults: defaults}}
		claim := &ext.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default"}, Spec: ext.SandboxClaimSpec{WarmPoolRef: ext.SandboxWarmPoolRef{Name: poolName}}}
		if err := live.Create(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if err := live.Create(ctx, claim); err != nil {
			t.Fatal(err)
		}
		return pool, claim
	}
	t.Run("inherited_delete_removes_claim", func(t *testing.T) {
		defaults := independentTTL(1)
		defaults.ShutdownPolicy = ext.ShutdownPolicyDelete
		pool, claim := setup(t, "delete", "claim-delete", defaults)
		key := client.ObjectKeyFromObject(claim)
		r := &SandboxClaimReconciler{Client: live, APIReader: live, Scheme: scheme, Tracer: asmetrics.NewNoOp(), DisableObservabilityAnnotations: true}
		if stop, err := r.initializeDefaultLifecycle(ctx, claim); stop || err != nil {
			t.Fatalf("resolve pool policy: stop=%v err=%v", stop, err)
		}
		var persisted ext.SandboxClaim
		if err := live.Get(ctx, key, &persisted); err != nil {
			t.Fatal(err)
		}
		wantDeadline := claim.CreationTimestamp.Add(time.Second)
		if persisted.Spec.Lifecycle != nil || persisted.Status.DefaultShutdownTime == nil ||
			!persisted.Status.DefaultShutdownTime.Time.Equal(wantDeadline) ||
			persisted.Status.DefaultShutdownPolicy != ext.ShutdownPolicyDelete {
			t.Fatalf("expected inherited deadline and Delete policy in persisted status; got %+v", persisted.Status)
		}
		pool.Spec.ClaimDefaults.ShutdownPolicy = ext.ShutdownPolicyRetain
		if err := live.Update(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if delay := time.Until(wantDeadline); delay > 0 {
			time.Sleep(delay)
		}
		// A fresh reconciler has to rely on persisted status, not the old pool or process memory.
		r = &SandboxClaimReconciler{Client: live, APIReader: live, Scheme: scheme, Tracer: asmetrics.NewNoOp(), DisableObservabilityAnnotations: true}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := live.Get(ctx, key, &persisted); err != nil || !hasClaimExpiredCondition(persisted.Status.Conditions) {
			t.Fatalf("expected expired Claim before deletion: %v, conditions=%+v", err, persisted.Status.Conditions)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := live.Get(ctx, key, &persisted); !k8errors.IsNotFound(err) {
			t.Fatalf("expected actual Claim deletion after inherited Delete: %v", err)
		}
	})

	t.Run("committed_status_response_lost", func(t *testing.T) {
		pool, claim := setup(t, "enabled", "claim-stale", independentTTL(1))
		// Force cold allocation if reconciliation ever passes the expiry guard.
		claim.Spec.Env = []ext.EnvVar{{Name: "TTL_TEST", Value: "integration"}}
		if err := live.Update(ctx, claim); err != nil {
			t.Fatal(err)
		}
		template := &ext.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "ttl-fixture", Namespace: "default"}, Spec: ext.SandboxTemplateSpec{EnvVarsInjectionPolicy: ext.EnvVarsInjectionPolicyAllowed, SandboxBlueprint: sandbox.SandboxBlueprint{PodTemplate: sandbox.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: "example.invalid/never-pulled"}}}}}}}
		if err := live.Create(ctx, template); err != nil {
			t.Fatal(err)
		}
		var hold atomic.Bool
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblockWatch := func() { releaseOnce.Do(func() { close(release) }) }
		cacheCfg := rest.CopyConfig(cfg)
		cacheCfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return ttlAPIRoundTripper(func(req *http.Request) (*http.Response, error) {
				res, err := next.RoundTrip(req)
				if err == nil && strings.HasSuffix(req.URL.Path, "/sandboxclaims") && req.URL.Query().Get("watch") == "true" {
					res.Body = ttlWatchGate{res.Body, req.Context(), &hold, release}
				}
				return res, err
			})
		})
		c, err := cache.New(cacheCfg, cache.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, stopCache := context.WithCancel(ctx)
		exited := make(chan error, 1)
		go func() { exited <- c.Start(waitCtx) }()
		defer func() {
			unblockWatch()
			stopCache()
			if err := <-exited; err != nil {
				t.Error(err)
			}
		}()
		if _, err = c.GetInformer(ctx, &ext.SandboxClaim{}); err != nil {
			t.Fatal(err)
		}
		if _, err = c.GetInformer(ctx, &ext.SandboxWarmPool{}); err != nil {
			t.Fatal(err)
		}
		if !c.WaitForCacheSync(ctx) {
			t.Fatal("informers did not sync")
		}
		var creates, patches, dropped atomic.Int32
		noAck := errors.New("test-only committed status PATCH response dropped")
		writeCfg := rest.CopyConfig(cfg)
		writeCfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return ttlAPIRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/sandboxes") {
					creates.Add(1)
				}
				res, err := next.RoundTrip(req)
				if err == nil && req.Method == http.MethodPatch &&
					strings.HasSuffix(req.URL.Path, "/sandboxclaims/"+claim.Name+"/status") &&
					patches.Add(1) == 1 {
					if res.StatusCode != http.StatusOK {
						return res, nil
					}
					// Consume the real successful response without exposing it to client-go.
					_, err := io.Copy(io.Discard, res.Body)
					closeErr := res.Body.Close()
					if err != nil || closeErr != nil {
						return nil, errors.Join(err, closeErr)
					}
					dropped.Add(1)
					return nil, noAck
				}
				return res, err
			})
		})
		viaCache, err := client.New(writeCfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
		if err != nil {
			t.Fatal(err)
		}
		claimKey := client.ObjectKeyFromObject(claim)
		poolKey := client.ObjectKeyFromObject(pool)
		var observed ext.SandboxClaim
		if err := c.Get(ctx, claimKey, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.Status.DefaultShutdownTime != nil {
			t.Fatal("test did not start with an unresolved cached Claim")
		}
		hold.Store(true)
		r := &SandboxClaimReconciler{Client: viaCache, APIReader: live, Scheme: scheme, Tracer: asmetrics.NewNoOp(), DisableObservabilityAnnotations: true}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: claimKey})
		if !errors.Is(err, noAck) || dropped.Load() != 1 || patches.Load() != 1 || creates.Load() != 0 {
			t.Fatalf("first reconcile did not see exactly one dropped response without creating: err=%v patches=%d dropped=%d creates=%d", err, patches.Load(), dropped.Load(), creates.Load())
		}
		var saved ext.SandboxClaim
		if err := live.Get(ctx, claimKey, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Status.DefaultShutdownTime == nil {
			t.Fatal("no real persisted deadline")
		}
		if !saved.Status.DefaultShutdownTime.Equal(&metav1.Time{Time: saved.CreationTimestamp.Add(time.Second)}) {
			t.Fatal("deadline is not creation+TTL")
		}
		pool.Spec.ClaimDefaults = nil
		if err := live.Update(ctx, pool); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(5 * time.Second)
		for {
			var cachedPool ext.SandboxWarmPool
			if err := c.Get(ctx, poolKey, &cachedPool); err != nil {
				t.Fatal(err)
			}
			if cachedPool.Spec.ClaimDefaults == nil && !time.Now().Before(saved.Status.DefaultShutdownTime.Time) {
				break
			}
			if time.Now().After(until) {
				t.Fatal("pool informer did not observe OFF after deadline")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := c.Get(ctx, claimKey, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.Status.DefaultShutdownTime != nil || observed.ResourceVersion == saved.ResourceVersion {
			t.Fatal("Claim watch did not stay stale; invalid negative control")
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: claimKey}); err != nil {
			t.Fatal(err)
		}
		if creates.Load() != 0 {
			t.Fatalf("issued %d Sandbox CREATE attempts during stale phase", creates.Load())
		}
		var actual sandbox.Sandbox
		if err := live.Get(ctx, claimKey, &actual); !k8errors.IsNotFound(err) {
			t.Fatalf("expired Claim provisioned before cached deadline caught up: %v", err)
		}
		var after ext.SandboxClaim
		if err := live.Get(ctx, claimKey, &after); err != nil {
			t.Fatal(err)
		}
		if !hasClaimExpiredCondition(after.Status.Conditions) || after.Status.DefaultShutdownTime == nil || !after.Status.DefaultShutdownTime.Equal(saved.Status.DefaultShutdownTime) {
			t.Fatalf("no persisted unchanged deadline and expiry evidence: %+v", after.Status)
		}
		// Permit real watch convergence and confirm normal later reconciliation
		// still keeps the original expiry and never starts allocation.
		hold.Store(false)
		unblockWatch()
		until = time.Now().Add(5 * time.Second)
		for {
			if err := c.Get(ctx, claimKey, &observed); err != nil {
				t.Fatal(err)
			}
			if observed.ResourceVersion == after.ResourceVersion && hasClaimExpiredCondition(observed.Status.Conditions) {
				break
			}
			if time.Now().After(until) {
				t.Fatal("released Claim watch did not converge to the observed expired state")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: claimKey}); err != nil {
			t.Fatal(err)
		}
		if err := live.Get(ctx, claimKey, &after); err != nil {
			t.Fatal(err)
		}
		if creates.Load() != 0 || !hasClaimExpiredCondition(after.Status.Conditions) || after.Status.DefaultShutdownTime == nil || !after.Status.DefaultShutdownTime.Equal(saved.Status.DefaultShutdownTime) {
			t.Fatalf("after watch catch-up: creates=%d unexpected status=%+v", creates.Load(), after.Status)
		}
		t.Log("Real API accepted status PATCH (HTTP 200); exactly one successful response was discarded. An independent GET confirmed the deadline before cache caught up.")
		// Independent positive control: on the same real server with the same
		// opt-out pool and template, remove only the persisted-deadline factor.
		control := &ext.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "control-cold", Namespace: "default"}, Spec: *after.Spec.DeepCopy()}
		if err := live.Create(ctx, control); err != nil {
			t.Fatal(err)
		}
		realController := &SandboxClaimReconciler{Client: live, APIReader: live, Scheme: scheme, Tracer: asmetrics.NewNoOp(), DisableObservabilityAnnotations: true}
		controlKey := client.ObjectKeyFromObject(control)
		if _, err := realController.Reconcile(ctx, ctrl.Request{NamespacedName: controlKey}); err != nil {
			t.Fatalf("positive cold control reconcile: %v", err)
		}
		if err := live.Get(ctx, controlKey, &actual); err != nil || !metav1.IsControlledBy(&actual, control) {
			t.Fatalf("positive control did not provision an owned Sandbox: %v", err)
		}

		t.Log("Real Claim watch was stale while pool cache saw OFF; complete Reconcile recorded expiry, preserved deadline and created no Sandbox.")
	})
}
