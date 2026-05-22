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

// Package cache maintains an in-memory map from Sandbox UID to Pod IP,
// populated by a Kubernetes Pod informer. It is the single piece of
// K8s-aware state in the sandbox-router; the ext_proc handler reads from
// it on every request to drive Envoy's ORIGINAL_DST cluster.
//
// The Sandbox UID is taken from the Pod's controller OwnerReference,
// not from a Pod label (the controller does not stamp the Sandbox UID as
// a label today; see sandbox-router/README.md "Why OwnerReferences" for
// the rationale).
package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// SandboxAPIGroup is the API group of the Sandbox CR. We match on this
	// + Kind="Sandbox" when inspecting Pod OwnerReferences to identify
	// sandbox-owned Pods regardless of which CRD version created them.
	SandboxAPIGroup = "agents.x-k8s.io"

	// SandboxKind is the kind of the controlling resource for sandbox Pods.
	SandboxKind = "Sandbox"

	// PodSandboxNameHashLabel is the label every sandbox-owned Pod carries
	// (its value is hash(sandbox.Name)). We use it as a label-selector
	// filter on the Pod informer so we only get events for sandbox Pods.
	//
	// The constant is duplicated here because the controller defines it as
	// a package-private string (controllers.sandboxLabel). Future work:
	// promote it to api/v1beta1 so we can import it directly.
	PodSandboxNameHashLabel = "agents.x-k8s.io/sandbox-name-hash"

	// defaultResync is the informer relist period. Short enough to catch
	// missed events; long enough to not hammer the API server. Matches
	// typical controller-runtime defaults.
	defaultResync = 10 * time.Minute
)

// Entry is a single cached mapping. Returned by Get so future fields
// (e.g. ready timestamp, ownership labels for authz) can be added without
// breaking call sites.
type Entry struct {
	// PodIP is the IPv4/IPv6 address of the sandbox Pod. The ext_proc
	// handler combines this with the inbound X-Sandbox-Port to build the
	// upstream target.
	PodIP string

	// SandboxName is the human-readable Sandbox CR name (== Pod.Name). Used
	// for logging and to construct DNS-form upstream targets on cache miss.
	SandboxName string

	// Namespace is the K8s namespace of the Sandbox / Pod.
	Namespace string
}

// Cache is a thread-safe Sandbox-UID → Entry map kept up to date by a
// background Pod informer. Lookups are O(1) and lock-free for the common
// path (RLock + map read).
type Cache struct {
	log      logr.Logger
	informer cache.SharedIndexInformer
	factory  informers.SharedInformerFactory
	stopOnce sync.Once
	stopCh   chan struct{}

	mu      sync.RWMutex
	entries map[types.UID]Entry
}

// Options configure the cache. Namespace is empty for cluster-wide
// (recommended for the router; sandbox Pods can live in many namespaces).
type Options struct {
	Client    kubernetes.Interface
	Log       logr.Logger
	Namespace string
	Resync    time.Duration
}

// New constructs a Cache backed by a filtered Pod SharedInformer. The
// informer is NOT started; call Start to launch it and WaitForSync to
// gate readiness.
func New(o Options) (*Cache, error) {
	if o.Client == nil {
		return nil, errors.New("cache: Client is required")
	}
	if o.Resync == 0 {
		o.Resync = defaultResync
	}
	// Server-side filter: only Pods carrying the sandbox-name-hash label.
	// Reduces informer memory and API traffic substantially in mixed
	// clusters where most Pods are NOT sandboxes.
	hashSel, err := labels.NewRequirement(PodSandboxNameHashLabel, selection.Exists, nil)
	if err != nil {
		return nil, err
	}
	tweak := func(opts *metav1.ListOptions) {
		opts.LabelSelector = labels.NewSelector().Add(*hashSel).String()
		// Skip Pods that have been scheduled but have no IP yet — they
		// can't be a useful routing target. Apiserver-side filter.
		opts.FieldSelector = fields.OneTermNotEqualSelector("status.podIP", "").String()
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		o.Client, o.Resync,
		informers.WithNamespace(o.Namespace),
		informers.WithTweakListOptions(tweak),
	)
	podInformer := factory.Core().V1().Pods().Informer()

	c := &Cache{
		log:      o.Log,
		informer: podInformer,
		factory:  factory,
		stopCh:   make(chan struct{}),
		entries:  make(map[types.UID]Entry),
	}

	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAddOrUpdate,
		UpdateFunc: func(_, newObj any) { c.onAddOrUpdate(newObj) },
		DeleteFunc: c.onDelete,
	}); err != nil {
		return nil, err
	}
	return c, nil
}

// Start launches the informer goroutines. Safe to call once; subsequent
// calls are no-ops.
func (c *Cache) Start(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.stop()
	}()
	c.factory.Start(c.stopCh)
}

// WaitForSync blocks until the informer's initial LIST has been
// processed, or ctx is canceled. Returns true on successful sync.
// The gRPC health check should gate SERVING on this.
func (c *Cache) WaitForSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced)
}

// HasSynced reports whether the informer has completed its initial LIST.
func (c *Cache) HasSynced() bool {
	return c.informer.HasSynced()
}

// Get looks up the cached entry for the given Sandbox UID. Returns
// (Entry, true) when known, (Entry{}, false) on cache miss. Lock-free in
// the common case via sync.RWMutex.
func (c *Cache) Get(uid types.UID) (Entry, bool) {
	c.mu.RLock()
	e, ok := c.entries[uid]
	c.mu.RUnlock()
	return e, ok
}

// Len returns the current number of cached entries. Primarily for tests
// and metrics; not on the request hot path.
func (c *Cache) Len() int {
	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	return n
}

func (c *Cache) stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// onAddOrUpdate is invoked by the informer for every Add and Update
// event. We extract the controlling Sandbox UID from OwnerReferences and
// store (or refresh) the entry only when the Pod is Ready and has an IP.
//
// A Pod that flips from Ready to NotReady is removed from the cache so
// requests don't get routed to a Pod that isn't accepting traffic.
func (c *Cache) onAddOrUpdate(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	uid, ok := sandboxUIDOf(pod)
	if !ok {
		return
	}
	if !podReady(pod) || pod.Status.PodIP == "" {
		c.remove(uid)
		return
	}
	c.upsert(uid, Entry{
		PodIP:       pod.Status.PodIP,
		SandboxName: pod.Name,
		Namespace:   pod.Namespace,
	})
}

func (c *Cache) onDelete(obj any) {
	// DeletedFinalStateUnknown wraps the last-known state when the
	// informer missed the delete event. Unwrap it.
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if uid, ok := sandboxUIDOf(pod); ok {
		c.remove(uid)
	}
}

func (c *Cache) upsert(uid types.UID, e Entry) {
	c.mu.Lock()
	prev, existed := c.entries[uid]
	c.entries[uid] = e
	c.mu.Unlock()
	if !existed {
		c.log.V(1).Info("cache add", "uid", uid, "pod", e.SandboxName, "ip", e.PodIP, "ns", e.Namespace)
	} else if prev.PodIP != e.PodIP {
		c.log.V(1).Info("cache update", "uid", uid, "pod", e.SandboxName, "ip", e.PodIP, "ns", e.Namespace, "prev_ip", prev.PodIP)
	}
}

func (c *Cache) remove(uid types.UID) {
	c.mu.Lock()
	_, existed := c.entries[uid]
	delete(c.entries, uid)
	c.mu.Unlock()
	if existed {
		c.log.V(1).Info("cache remove", "uid", uid)
	}
}

// Invalidate evicts the entry for uid if present, returning true when an
// entry was actually removed. This is the public hook the KEP's "active
// cache invalidation on connection error" calls for: the proxy invokes it
// from its ErrorHandler when a dispatch using the cached IP fails, so
// the next request for the same UID falls through to DNS while the
// informer catches up.
//
// Safe to call for an unknown UID — the operation is a no-op.
func (c *Cache) Invalidate(uid types.UID) bool {
	c.mu.Lock()
	_, existed := c.entries[uid]
	delete(c.entries, uid)
	c.mu.Unlock()
	if existed {
		c.log.V(1).Info("cache invalidated by caller", "uid", uid)
	}
	return existed
}

// sandboxUIDOf extracts the Sandbox CR UID from a Pod's controller
// OwnerReference. Returns ("", false) for Pods not owned by a Sandbox
// (which shouldn't reach us thanks to the label filter, but the check is
// cheap insurance against stray events).
func sandboxUIDOf(pod *corev1.Pod) (types.UID, bool) {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Kind != SandboxKind {
			continue
		}
		// OwnerReference.APIVersion looks like "agents.x-k8s.io/v1beta1";
		// match the group prefix so we don't break across API versions.
		if !apiVersionInGroup(ref.APIVersion, SandboxAPIGroup) {
			continue
		}
		return ref.UID, true
	}
	return "", false
}

// apiVersionInGroup reports whether apiVersion ("group/version") is in
// the given group. Handles versionless input gracefully.
func apiVersionInGroup(apiVersion, group string) bool {
	if apiVersion == group {
		return true
	}
	for i := 0; i < len(apiVersion); i++ {
		if apiVersion[i] == '/' {
			return apiVersion[:i] == group
		}
	}
	return false
}

// podReady reports whether the Pod's status carries a PodReady=True
// condition. We require Ready (not just Running) because containers may
// still be initializing or failing probes; routing to such a Pod produces
// noisy 502s for the caller.
func podReady(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
