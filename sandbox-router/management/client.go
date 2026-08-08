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

package management

import (
	"context"
	"fmt"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// idempotencyKeyLabel is the SandboxClaim metadata label that stores the
// caller-supplied idempotency key. Using a label lets the API server filter
// on it efficiently without a full list-and-scan.
const idempotencyKeyLabel = "sandbox.intapp.com/idempotency-key"

// Client wraps a controller-runtime client scoped to SandboxClaim operations.
type Client struct {
	client           runtimeclient.Client
	defaultNamespace string
}

// New builds a controller-runtime client with the SandboxClaim scheme registered.
func New(restConfig *rest.Config, defaultNamespace string) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := extensionsv1beta1.SchemeBuilder.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add extensions v1beta1 to scheme: %w", err)
	}

	c, err := runtimeclient.New(restConfig, runtimeclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build controller-runtime client: %w", err)
	}
	return &Client{client: c, defaultNamespace: defaultNamespace}, nil
}

// Create creates a new SandboxClaim from the given request. The object name is
// server-generated via GenerateName.
//
// If req.IdempotencyKey is set, Create first checks whether a SandboxClaim
// carrying that key already exists in the namespace and returns it immediately
// if found. This makes POST /v1/sandboxes safe to retry after a lost response.
func (c *Client) Create(ctx context.Context, req *CreateSandboxRequest) (*extensionsv1beta1.SandboxClaim, error) {
	ns := req.Namespace
	if ns == "" {
		ns = c.defaultNamespace
	}

	if req.IdempotencyKey != "" {
		existing, err := c.findByIdempotencyKey(ctx, ns, req.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("check idempotency key: %w", err)
		}
		if existing != nil {
			return existing, nil
		}
	}

	labels := map[string]string{}
	if req.IdempotencyKey != "" {
		labels[idempotencyKeyLabel] = req.IdempotencyKey
	}

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sandbox-",
			Namespace:    ns,
			Labels:       labels,
		},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{
				Name: req.WarmPool,
			},
		},
	}

	if req.TTLSeconds != nil {
		shutdownTime := metav1.NewTime(time.Now().Add(time.Duration(*req.TTLSeconds) * time.Second))
		claim.Spec.Lifecycle = &extensionsv1beta1.Lifecycle{
			ShutdownTime:   &shutdownTime,
			ShutdownPolicy: extensionsv1beta1.ShutdownPolicyDelete,
		}
	}

	if len(req.Env) > 0 {
		for _, e := range req.Env {
			claim.Spec.Env = append(claim.Spec.Env, extensionsv1beta1.EnvVar{
				Name:          e.Name,
				Value:         e.Value,
				ContainerName: e.ContainerName,
			})
		}
	}

	if len(req.Labels) > 0 || len(req.Annotations) > 0 {
		claim.Spec.AdditionalPodMetadata.Labels = req.Labels
		claim.Spec.AdditionalPodMetadata.Annotations = req.Annotations
	}

	if err := c.client.Create(ctx, claim); err != nil {
		return nil, fmt.Errorf("create SandboxClaim: %w", err)
	}
	return claim, nil
}

// Get retrieves a SandboxClaim by namespace and name.
func (c *Client) Get(ctx context.Context, namespace, name string) (*extensionsv1beta1.SandboxClaim, error) {
	claim := &extensionsv1beta1.SandboxClaim{}
	key := runtimeclient.ObjectKey{Namespace: namespace, Name: name}
	if err := c.client.Get(ctx, key, claim); err != nil {
		return nil, err
	}
	return claim, nil
}

// List returns all SandboxClaims in the given namespace. If namespace is empty,
// all namespaces are searched.
func (c *Client) List(ctx context.Context, namespace string) (*extensionsv1beta1.SandboxClaimList, error) {
	list := &extensionsv1beta1.SandboxClaimList{}
	opts := []runtimeclient.ListOption{}
	if namespace != "" {
		opts = append(opts, runtimeclient.InNamespace(namespace))
	}
	if err := c.client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list SandboxClaims: %w", err)
	}
	return list, nil
}

// Delete removes a SandboxClaim. It returns nil if the object was not found.
func (c *Client) Delete(ctx context.Context, namespace, name string) error {
	claim := &extensionsv1beta1.SandboxClaim{}
	claim.Name = name
	claim.Namespace = namespace
	if err := c.client.Delete(ctx, claim); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete SandboxClaim %s/%s: %w", namespace, name, err)
	}
	return nil
}

// findByIdempotencyKey returns the first SandboxClaim in namespace whose
// idempotencyKeyLabel matches key, or nil if none exists.
func (c *Client) findByIdempotencyKey(ctx context.Context, namespace, key string) (*extensionsv1beta1.SandboxClaim, error) {
	list := &extensionsv1beta1.SandboxClaimList{}
	opts := []runtimeclient.ListOption{
		runtimeclient.InNamespace(namespace),
		runtimeclient.MatchingLabels{idempotencyKeyLabel: key},
	}
	if err := c.client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list SandboxClaims by idempotency key: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// toResponse converts a SandboxClaim into an API response. The Ready condition
// drives the Status field; ConnectionInfo is populated when the claim is Ready
// and a Sandbox name has been assigned.
func (c *Client) toResponse(claim *extensionsv1beta1.SandboxClaim) *SandboxResponse {
	resp := &SandboxResponse{
		ID:          claim.Name,
		Namespace:   claim.Namespace,
		SandboxName: claim.Status.SandboxStatus.Name,
		Status:      "Pending",
		CreatedAt:   claim.CreationTimestamp.Time,
	}

	// Map conditions into ConditionInfo and determine overall status.
	for _, cond := range claim.Status.Conditions {
		resp.Conditions = append(resp.Conditions, ConditionInfo{
			Type:    cond.Type,
			Status:  string(cond.Status),
			Reason:  cond.Reason,
			Message: cond.Message,
		})
	}

	readyCond := meta.FindStatusCondition(claim.Status.Conditions, "Ready")
	if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
		resp.Status = "Ready"
		resp.Ready = true
	}

	if resp.Ready && resp.SandboxName != "" {
		resp.Connection = &ConnectionInfo{
			SandboxID:        resp.SandboxName,
			SandboxNamespace: resp.Namespace,
			Headers: map[string]string{
				"X-Sandbox-ID":        resp.SandboxName,
				"X-Sandbox-Namespace": resp.Namespace,
			},
		}
	}

	return resp
}
