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

package managedsandbox

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/examples/managed-sandbox/api/v1alpha1"
)

// GatewayParentRef identifies the Gateway resource that owns the routes
// produced by the sandbox controller. For now it is a controller-wide
// constant (configurable via ManagedSandboxReconciler.GatewayParent). Per-tenant
// Gateway selection can be added later.
type GatewayParentRef struct {
	// Namespace of the Gateway. If empty, sandboxes' own namespace is used.
	Namespace string
	// Name of the Gateway.
	Name string
	// Listener section name (optional).
	SectionName string
}

// reconcileHTTPRoute ensures an HTTPRoute exists for a multi-tenant
// sandbox, routing `/s/<sandbox-uid>/...` to the pool pod's per-pod
// Service. Status.Endpoints is updated on success.
//
// Skipped (no-op) if the controller's GatewayParent is unset.
func (r *ManagedSandboxReconciler) reconcileHTTPRoute(ctx context.Context, sandbox *sandboxv1alpha1.ManagedSandbox) error {
	if r.GatewayParent == nil || r.GatewayParent.Name == "" {
		return nil
	}
	if sandbox.Status.Host == nil || sandbox.Status.Host.PodName == "" {
		// Nothing to route to yet.
		return nil
	}

	desired := r.buildHTTPRoute(sandbox)
	existing := &gwv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case k8serrors.IsNotFound(err):
		if err := ctrl.SetControllerReference(sandbox, desired, r.Scheme, ctrlutil.WithBlockOwnerDeletion(false)); err != nil {
			return fmt.Errorf("httproute: set owner: %w", err)
		}
		if err := r.Create(ctx, desired, client.FieldOwner(managedSandboxControllerFieldOwner)); err != nil {
			return fmt.Errorf("httproute: create: %w", err)
		}
	case err != nil:
		return fmt.Errorf("httproute: get: %w", err)
	default:
		existing.Spec = desired.Spec
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range desired.Labels {
			existing.Labels[k] = v
		}
		if err := ctrl.SetControllerReference(sandbox, existing, r.Scheme, ctrlutil.WithBlockOwnerDeletion(false)); err != nil {
			return fmt.Errorf("httproute: set owner: %w", err)
		}
		if err := r.Update(ctx, existing, client.FieldOwner(managedSandboxControllerFieldOwner)); err != nil {
			return fmt.Errorf("httproute: update: %w", err)
		}
	}

	pathPrefix := fmt.Sprintf("/s/%s/", sandbox.UID)
	sandbox.Status.Endpoints = []sandboxv1alpha1.SandboxEndpoint{
		{Name: "http", URL: pathPrefix},
	}
	return nil
}

func (r *ManagedSandboxReconciler) buildHTTPRoute(sandbox *sandboxv1alpha1.ManagedSandbox) *gwv1.HTTPRoute {

	pathPrefix := fmt.Sprintf("/s/%s/", sandbox.UID)
	pathType := gwv1.PathMatchPathPrefix
	backendPort := gwv1.PortNumber(8080)

	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
			Labels:    map[string]string{sandboxLabel: NameHash(sandbox.Name)},
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{*r.GatewayParent},
			},
			Rules: []gwv1.HTTPRouteRule{{
				Matches: []gwv1.HTTPRouteMatch{{
					Path: &gwv1.HTTPPathMatch{
						Type:  &pathType,
						Value: &pathPrefix,
					},
				}},
				BackendRefs: []gwv1.HTTPBackendRef{{
					BackendRef: gwv1.BackendRef{
						// Backend = headless Service that the Provisioner
						// creates per pool pod when CreateService is on
						// (set when --gateway-name is configured). Name
						// matches the pod's, so this is the right target.
						BackendObjectReference: gwv1.BackendObjectReference{
							Name: gwv1.ObjectName(sandbox.Status.Host.PodName),
							Port: &backendPort,
						},
					},
				}},
			}},
		},
	}
}
