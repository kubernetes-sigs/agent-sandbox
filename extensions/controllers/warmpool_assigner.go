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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	v1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type WarmPoolAssigner struct {
	client.Client
	mu       sync.RWMutex
	Pools    map[string]chan types.NamespacedName
	InFlight sync.Map
}

func (w *WarmPoolAssigner) SetupWithManager(mgr ctrl.Manager) error {
	sandboxInformer, err := mgr.GetCache().GetInformer(context.Background(), &v1alpha1.Sandbox{})
	if err != nil {
		return err
	}

	_, err = sandboxInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			w.handleSandboxEvent(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			w.handleSandboxEvent(newObj)
		},
	})

	return err
}

func (w *WarmPoolAssigner) GetOrCreatePool(ctx context.Context, hash string) chan types.NamespacedName {
	w.mu.RLock()
	ch, exists := w.Pools[hash]
	w.mu.RUnlock()
	if exists {
		return ch
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if ch, exists := w.Pools[hash]; exists {
		return ch
	}

	ch = make(chan types.NamespacedName, 1000)
	w.Pools[hash] = ch

	var sandboxes v1alpha1.SandboxList
	if err := w.Client.List(ctx, &sandboxes, client.MatchingLabels{"agents.x-k8s.io/sandbox-template-ref-hash": hash}); err == nil {
		for _, sb := range sandboxes.Items {
			// Ignore deleting pods
			if !sb.DeletionTimestamp.IsZero() {
				continue
			}

			// Must be unowned (or owned by the WarmPool, not a Claim)
			controllerRef := metav1.GetControllerOf(&sb)
			if controllerRef != nil && controllerRef.Kind == "SandboxClaim" {
				continue
			}

			// Must be Ready
			isReady := false
			for _, cond := range sb.Status.Conditions {
				if cond.Type == string(v1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
					isReady = true
					break
				}
			}

			if isReady {
				if _, queued := w.InFlight.Load(sb.Name); !queued {
					select {
					case ch <- types.NamespacedName{Name: sb.Name, Namespace: sb.Namespace}:
						w.InFlight.Store(sb.Name, true)
					default:
					}
				}
			}
		}
	}

	return ch
}

func (w *WarmPoolAssigner) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (w *WarmPoolAssigner) handleSandboxEvent(obj interface{}) {
	sandbox, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}

	if !sandbox.DeletionTimestamp.IsZero() {
		return
	}

	controllerRef := metav1.GetControllerOf(sandbox)
	if controllerRef == nil || controllerRef.Kind != "SandboxWarmPool" {
		return
	}

	isReady := false
	for _, cond := range sandbox.Status.Conditions {
		if cond.Type == string(v1alpha1.SandboxConditionReady) && cond.Status == metav1.ConditionTrue {
			isReady = true
			break
		}
	}

	if isReady {
		templateHash, hasLabel := sandbox.Labels["agents.x-k8s.io/sandbox-template-ref-hash"]
		if !hasLabel {
			return
		}

		w.mu.RLock()
		ch, exists := w.Pools[templateHash]
		w.mu.RUnlock()

		if !exists {
			return
		}

		if _, inFlight := w.InFlight.Load(sandbox.Name); !inFlight {
			select {
			case ch <- types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}:
				w.InFlight.Store(sandbox.Name, true)
			default:
			}
		}
	}
}
