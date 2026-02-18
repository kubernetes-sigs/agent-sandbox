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

package framework

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// Subscription represents a subscription to events from a ResourceWatch.
type Subscription struct {
	id uint64

	// filter allows us to use a broader watch and filter events per subscription.
	filter WatchFilter

	resourceWatch *ResourceWatch

	events *ConcurrentQueue[watch.Event]
}

// Next waits for and returns the next event.
// It returns an error if the context is cancelled or the subscription is closed.
func (s *Subscription) Next(ctx context.Context) (watch.Event, error) {
	event, err := s.events.Wait(ctx)
	if err != nil {
		return watch.Event{}, err
	}

	return event, nil
}

// ResourceWatch maintains a watch for a specific GVR+namespace combination.
type ResourceWatch struct {
	gvr       schema.GroupVersionResource
	namespace string // empty for cluster-scoped resources

	mu            sync.RWMutex
	subscriptions map[uint64]*Subscription
	nextSubID     uint64

	dynamicClient dynamic.Interface

	// cancelWatchLoop will cancel the watch loop
	cancelWatchLoop context.CancelFunc
}

// WatchSet maintains persistent watches for resource types.
type WatchSet struct {
	mu            sync.RWMutex
	watches       map[watchKey]*ResourceWatch
	dynamicClient dynamic.Interface
}

// watchKey identifies a unique watch by GVR and namespace.
type watchKey struct {
	gvr       schema.GroupVersionResource
	namespace string
}

// NewWatchSet creates a new WatchSet.
func NewWatchSet(dynamicClient dynamic.Interface) *WatchSet {
	return &WatchSet{
		watches:       make(map[watchKey]*ResourceWatch),
		dynamicClient: dynamicClient,
	}
}

// getOrCreateWatch returns an existing watch or creates a new one.
func (ws *WatchSet) getOrCreateWatch(ctx context.Context, gvr schema.GroupVersionResource, namespace string) *ResourceWatch {
	key := watchKey{gvr: gvr, namespace: namespace}

	// Try read lock first
	ws.mu.RLock()
	rw, ok := ws.watches[key]
	ws.mu.RUnlock()
	if ok {
		return rw
	}

	// Need write lock to create
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Double-check after acquiring write lock
	if rw, ok := ws.watches[key]; ok {
		return rw
	}

	// ctx, cancel := context.WithCancel(context.Background())
	rw = &ResourceWatch{
		gvr:           gvr,
		namespace:     namespace,
		subscriptions: make(map[uint64]*Subscription),
		dynamicClient: ws.dynamicClient,
	}

	ctx, cancel := context.WithCancel(ctx)
	rw.cancelWatchLoop = cancel

	go rw.watchLoop(ctx)

	ws.watches[key] = rw
	return rw
}

// Subscribe creates a subscription to events for a specific object key.
// The key should be "namespace/name" for namespaced resources or just "name" for cluster-scoped.
// Returns a Subscription that receives events matching the filter.
func (ws *WatchSet) Subscribe(ctx context.Context, gvr schema.GroupVersionResource, filter WatchFilter) *Subscription {
	watchNamespace := ""
	if filter.Namespace != "" {
		watchNamespace = filter.Namespace
	}
	rw := ws.getOrCreateWatch(ctx, gvr, watchNamespace)
	return rw.subscribe(filter)
}

// Close removes a subscription
func (s *Subscription) Close() {
	s.resourceWatch.unsubscribe(s)
}

// Close stops all watches and cleans up resources.
func (ws *WatchSet) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for _, rw := range ws.watches {
		rw.stop()
	}
	ws.watches = nil
}

type WatchFilter struct {
	// Namespace is the namespace to filter on; empty means all namespaces.
	Namespace string
	// Name is the name to filter on; empty means all names.
	Name string
}

// subscribe adds a new subscription with the given key filter.
func (rw *ResourceWatch) subscribe(filter WatchFilter) *Subscription {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	sub := &Subscription{
		id:            rw.nextSubID,
		filter:        filter,
		resourceWatch: rw,
		events:        NewConcurrentQueue[watch.Event](),
	}
	rw.nextSubID++
	rw.subscriptions[sub.id] = sub

	return sub
}

// unsubscribe removes a subscription.
func (rw *ResourceWatch) unsubscribe(sub *Subscription) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if _, ok := rw.subscriptions[sub.id]; ok {
		delete(rw.subscriptions, sub.id)

		sub.events.Close(fmt.Errorf("subscription closed"))
	}

	// TODO: Stop the watch if there are no more subscriptions
}

// stop cancels the watch and closes all subscriptions.
func (rw *ResourceWatch) stop() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	rw.cancelWatchLoop()

	for _, sub := range rw.subscriptions {
		sub.events.Close(fmt.Errorf("subscription closed"))
	}
	rw.subscriptions = nil
}

// watchLoop runs the watch and broadcasts events to subscriptions.
func (rw *ResourceWatch) watchLoop(ctx context.Context) {
	var resourceVersion string

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Create the watch
		var resourceInterface dynamic.ResourceInterface
		if rw.namespace != "" {
			resourceInterface = rw.dynamicClient.Resource(rw.gvr).Namespace(rw.namespace)
		} else {
			resourceInterface = rw.dynamicClient.Resource(rw.gvr)
		}

		listOptions := metav1.ListOptions{
			Watch:           true,
			ResourceVersion: resourceVersion,
		}

		watcher, err := resourceInterface.Watch(ctx, listOptions)
		if err != nil {
			// If context is done, exit
			select {
			case <-ctx.Done():
				return
			default:
				// Wait a bit before retrying
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		// Process events
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return

			case event, ok := <-watcher.ResultChan():
				if !ok {
					// Watch channel closed, restart with last resourceVersion
					break
				}

				if event.Type == watch.Error {
					// On error, restart from scratch
					resourceVersion = ""
					break
				}

				// Broadcast to matching subscriptions
				rw.broadcast(event)
			}
		}
	}
}

// broadcast sends an event to all matching subscriptions.
func (rw *ResourceWatch) broadcast(event watch.Event) {
	name := ""
	namespace := ""

	if event.Object != nil {
		if typedObject, ok := event.Object.(metav1.Object); ok {
			name = typedObject.GetName()
			namespace = typedObject.GetNamespace()
		} else {
			klog.Warningf("broadcast: event object does not implement metav1.Object")
		}
	}

	rw.mu.RLock()
	defer rw.mu.RUnlock()

	for _, sub := range rw.subscriptions {
		switch event.Type {
		case watch.Error, watch.Bookmark:
			// Always send errors and bookmarks
		default:
			// Check if subscription filter matches
			if sub.filter.Namespace != "" && sub.filter.Namespace != namespace {
				continue
			}
			if sub.filter.Name != "" && sub.filter.Name != name {
				continue
			}
		}

		// Send event
		sub.events.Push(event)
	}
}

// ConcurrentQueue is a thread-safe queue.
type ConcurrentQueue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	closed error
}

func NewConcurrentQueue[T any]() *ConcurrentQueue[T] {
	l := &ConcurrentQueue[T]{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Close marks the queue as closed and wakes up all waiters.
func (l *ConcurrentQueue[T]) Close(err error) {
	l.mu.Lock()
	l.closed = err
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *ConcurrentQueue[T]) Push(item T) {
	l.mu.Lock()
	l.items = append(l.items, item)
	l.cond.Broadcast()
	l.mu.Unlock()
}

// contextTimeout is called when any context waiting on the condition is cancelled.
// It wakes up all waiters so they can check their context.
// We make this a method because that _should_ be more efficient than a closure (?)
func (l *ConcurrentQueue[T]) contextTimeout() {
	l.mu.Lock()
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *ConcurrentQueue[T]) Wait(ctx context.Context) (T, error) {
	var zeroT T

	l.mu.Lock()
	defer l.mu.Unlock()

	stop := context.AfterFunc(ctx, l.contextTimeout)
	defer stop()

	for len(l.items) == 0 {
		l.cond.Wait()

		if ctx.Err() != nil {
			return zeroT, ctx.Err()
		}

		if l.closed != nil {
			return zeroT, l.closed
		}
	}

	item := l.items[0]
	l.items[0] = zeroT // Help GC
	l.items = l.items[1:]
	return item, nil
}
