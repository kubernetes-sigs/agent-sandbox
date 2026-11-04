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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

func newWarmPoolDeletionFixture(replicas int32) (*extensionsv1beta1.SandboxWarmPool, *extensionsv1beta1.SandboxTemplate, *sandboxv1beta1.Sandbox) {
	pool, template := newOverCreationFixture(replicas)
	sb := createPoolSandbox(pool.Name, pool.Namespace, sandboxcontrollers.NameHash(pool.Name), template, "-member")
	sb.UID = "sandbox-uid"
	sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	sb.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, extensionsv1beta1.GroupVersion.WithKind("SandboxWarmPool"))}
	return pool, template, sb
}

func checkObservedWarmPoolDelete(obj client.Object, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		return errors.New("inventory deletion must carry UID and resourceVersion preconditions")
	}
	if *options.Preconditions.UID != obj.GetUID() || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
		return fmt.Errorf("DELETE preconditions must match observed UID %q and resourceVersion %q", obj.GetUID(), obj.GetResourceVersion())
	}
	return nil
}

func TestWarmPoolStaleDeletionConflictPreservesCapacity(t *testing.T) {
	ctx := context.Background()
	pool, template, sb := newWarmPoolDeletionFixture(1)
	pool.Spec.UpdateStrategy = &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType}
	sb.Labels[sandboxTemplateRefHash] = "old-template"
	scheme := newTestScheme()
	base := newFakeClient(scheme, pool, template, sb)
	observed := &sandboxv1beta1.Sandbox{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(sb), observed))
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := checkObservedWarmPoolDelete(observed, opts...); err != nil {
				return err
			}
			return k8serrors.NewConflict(schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), errors.New("ownership changed"))
		},
	})
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.True(t, k8serrors.IsConflict(err))
	current := &extensionsv1beta1.SandboxWarmPool{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(pool), current))
	require.Equal(t, int32(1), current.Status.Replicas, "failed deletion must retain the observed capacity")
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, base.List(ctx, list))
	require.Len(t, list.Items, 1, "409 must not create a replacement for an undeleted sandbox")
	require.True(t, r.exp().SatisfiedExpectations(warmPoolIdentity{NamespacedName: client.ObjectKeyFromObject(pool), UID: pool.UID}), "failed DELETE must cancel its expectation")
}

func TestWarmPoolSurplusDeletionUsesObservedIdentity(t *testing.T) {
	ctx := context.Background()
	pool, template, sb := newWarmPoolDeletionFixture(0)
	scheme := newTestScheme()
	base := newFakeClient(scheme, pool, template, sb)
	observed := &sandboxv1beta1.Sandbox{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(sb), observed))
	deleted := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := checkObservedWarmPoolDelete(observed, opts...); err != nil {
				return err
			}
			deleted = true
			return c.Delete(ctx, obj, opts...)
		},
	})
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	require.True(t, deleted)
	require.True(t, r.exp().IsPendingDeletion(warmPoolIdentity{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, UID: pool.UID}, observed.UID),
		"successful DELETE must wait for its watch observation")
	h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
	h.Delete(ctx, event.DeleteEvent{Object: observed}, nil)
	require.True(t, r.exp().SatisfiedExpectations(warmPoolIdentity{NamespacedName: client.ObjectKeyFromObject(pool), UID: pool.UID}))
}

func configureWarmPoolDeletionPath(path string, pool *extensionsv1beta1.SandboxWarmPool, sb *sandboxv1beta1.Sandbox) {
	switch path {
	case "surplus":
		zero := int32(0)
		pool.Spec.Replicas = &zero
	case "stale":
		pool.Spec.UpdateStrategy = &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType}
		sb.Labels[sandboxTemplateRefHash] = "old-template"
	case "stuck":
		sb.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	}
}

func TestWarmPoolDeletionProtectsConcurrentOwnershipChanges(t *testing.T) {
	for _, path := range []string{"surplus", "stale", "stuck"} {
		for _, change := range []string{"claim-adoption", "same-name-recreation"} {
			t.Run(path+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				pool, template, sb := newWarmPoolDeletionFixture(1)
				configureWarmPoolDeletionPath(path, pool, sb)
				if change == "same-name-recreation" {
					// Reuse the fake client's CREATE resourceVersion so this
					// case requires the UID precondition independently of RV.
					sb.ResourceVersion = "1"
				}
				scheme := newTestScheme()
				base := newFakeClient(scheme, pool, template, sb)
				changed := false
				var replacementUID types.UID
				c := interceptor.NewClient(base, interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if !changed {
							live := &sandboxv1beta1.Sandbox{}
							if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
								return err
							}
							before := live.DeepCopy()
							claim := &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "consumer", UID: "claim-uid"}}
							live.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))}
							delete(live.Labels, warmPoolSandboxLabel)
							if change == "claim-adoption" {
								if err := c.Patch(ctx, live, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
									return err
								}
							} else {
								if err := c.Delete(ctx, before); err != nil {
									return err
								}
								live.UID = "replacement-sandbox-uid"
								live.ResourceVersion = ""
								if err := c.Create(ctx, live); err != nil {
									return err
								}
								if live.ResourceVersion != before.ResourceVersion {
									return errors.New("recreation fixture must preserve RV to isolate the UID precondition")
								}
								replacementUID = live.UID
							}
							changed = true
						}
						if replacementUID != "" {
							// The fake client checks RV but does not enforce UID;
							// model that API-server check at the client boundary.
							options := (&client.DeleteOptions{}).ApplyOptions(opts)
							if options.Preconditions != nil && options.Preconditions.UID != nil && *options.Preconditions.UID != replacementUID {
								return k8serrors.NewConflict(schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), errors.New("UID precondition does not match replacement"))
							}
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
				r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
				require.True(t, k8serrors.IsConflict(err), "DELETE must reject the stale inventory observation")
				survivor := &sandboxv1beta1.Sandbox{}
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(sb), survivor))
				require.Equal(t, types.UID("claim-uid"), metav1.GetControllerOf(survivor).UID)
				if change == "same-name-recreation" {
					require.Equal(t, types.UID("replacement-sandbox-uid"), survivor.UID)
				}
				list := &sandboxv1beta1.SandboxList{}
				require.NoError(t, base.List(ctx, list))
				require.Len(t, list.Items, 1, "409 must preserve capacity through this observation")
				current := &extensionsv1beta1.SandboxWarmPool{}
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(pool), current))
				require.Equal(t, int32(1), current.Status.Replicas)
				require.True(t, r.exp().SatisfiedExpectations(warmPoolIdentity{NamespacedName: client.ObjectKeyFromObject(pool), UID: pool.UID}))
			})
		}
	}
}

func TestWarmPoolDeletionFailuresDoNotBlockRetry(t *testing.T) {
	for _, path := range []string{"surplus", "stale", "stuck"} {
		for _, failure := range []string{"conflict", "not-found", "unavailable"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				pool, template, sb := newWarmPoolDeletionFixture(1)
				configureWarmPoolDeletionPath(path, pool, sb)
				scheme := newTestScheme()
				base := newFakeClient(scheme, pool, template, sb)
				failDelete := true
				c := interceptor.NewClient(base, interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if !failDelete {
							return c.Delete(ctx, obj, opts...)
						}
						resource := schema.GroupResource{Group: sandboxv1beta1.GroupVersion.Group, Resource: "sandboxes"}
						switch failure {
						case "conflict":
							return k8serrors.NewConflict(resource, obj.GetName(), errors.New("ownership changed"))
						case "not-found":
							if err := c.Delete(ctx, obj); err != nil {
								return err
							}
							return k8serrors.NewNotFound(resource, obj.GetName())
						default:
							return k8serrors.NewServiceUnavailable("API server unavailable")
						}
					},
				})
				r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
				_, err := r.Reconcile(ctx, req)
				if failure != "not-found" {
					require.Error(t, err)
				}
				require.False(t, r.exp().IsPendingDeletion(warmPoolIdentity{NamespacedName: req.NamespacedName, UID: pool.UID}, sb.UID))
				require.True(t, r.exp().SatisfiedExpectations(warmPoolIdentity{NamespacedName: req.NamespacedName, UID: pool.UID}), "failed DELETE must not wait for a watch event")
				list := &sandboxv1beta1.SandboxList{}
				require.NoError(t, base.List(ctx, list))
				if failure == "not-found" {
					require.Empty(t, list.Items)
				} else {
					require.Len(t, list.Items, 1, "failed deletion must not release capacity")
					failDelete = false
					_, err = r.Reconcile(ctx, req)
					require.NoError(t, err)
					require.True(t, r.exp().IsPendingDeletion(warmPoolIdentity{NamespacedName: req.NamespacedName, UID: pool.UID}, sb.UID))
					h := &warmPoolSandboxEventHandler{EventHandler: handler.Funcs{}, expectations: r.exp()}
					h.Delete(ctx, event.DeleteEvent{Object: sb}, nil)
				}
				current := &extensionsv1beta1.SandboxWarmPool{}
				require.NoError(t, base.Get(ctx, req.NamespacedName, current))
				one := int32(1)
				current.Spec.Replicas = &one
				require.NoError(t, base.Update(ctx, current))
				_, err = r.Reconcile(ctx, req)
				require.NoError(t, err)
				require.NoError(t, base.List(ctx, list))
				require.Len(t, list.Items, 1, "fresh observation must allow replenishment")
			})
		}
	}
}

func TestWarmPoolStuckDeletionUsesObservedIdentity(t *testing.T) {
	ctx := context.Background()
	pool, template, sb := newWarmPoolDeletionFixture(1)
	sb.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	scheme := newTestScheme()
	base := newFakeClient(scheme, pool, template, sb)
	observed := &sandboxv1beta1.Sandbox{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(sb), observed))
	deleted := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := checkObservedWarmPoolDelete(observed, opts...); err != nil {
				return err
			}
			deleted = true
			return c.Delete(ctx, obj, opts...)
		},
	})
	r := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	require.NoError(t, err)
	require.True(t, deleted)
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, base.List(ctx, list))
	require.Empty(t, list.Items, "successful DELETE must not create a same-pass replacement")
	require.True(t, r.exp().IsPendingDeletion(warmPoolIdentity{NamespacedName: client.ObjectKeyFromObject(pool), UID: pool.UID}, observed.UID))
}
