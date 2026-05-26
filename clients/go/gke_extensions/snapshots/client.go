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

package snapshots

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	sandbox "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// crdDiscoverer is satisfied by discovery.DiscoveryInterface and any test fake.
type crdDiscoverer interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// PodSnapshotClient wraps sandbox.Client and returns SandboxWithSnapshotSupport
// instances. It validates at construction time that the GKE Pod Snapshot CRD
// is installed on the cluster.
type PodSnapshotClient struct {
	inner *sandbox.Client
	k8s   *sandbox.K8sHelper
	log   logr.Logger
	opts  sandbox.Options
}

// NewPodSnapshotClient creates a PodSnapshotClient. It fails immediately if the
// PodSnapshot CRD (podsnapshot.gke.io/v1) is not installed and accessible on
// the cluster.
func NewPodSnapshotClient(ctx context.Context, opts sandbox.Options) (*PodSnapshotClient, error) {
	// Build a shared K8sHelper so that both the inner sandbox.Client and the
	// snapshot wrappers share a single set of Kubernetes clients.
	k8s, err := sandbox.NewK8sHelper(opts.RestConfig, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("snapshots: failed to initialise Kubernetes clients: %w", err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(k8s.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("snapshots: failed to create discovery client: %w", err)
	}

	if err := checkSnapshotCRDInstalled(dc, opts.Logger); err != nil {
		return nil, err
	}

	opts.K8sHelper = k8s
	inner, err := sandbox.NewClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("snapshots: failed to create sandbox client: %w", err)
	}

	return &PodSnapshotClient{
		inner: inner,
		k8s:   k8s,
		log:   opts.Logger,
		opts:  opts,
	}, nil
}

// CreateSandbox provisions a new sandbox and wraps it with snapshot support.
func (c *PodSnapshotClient) CreateSandbox(ctx context.Context, template, namespace string) (*SandboxWithSnapshotSupport, error) {
	sb, err := c.inner.CreateSandbox(ctx, template, namespace)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "default"
	}
	return NewSandboxWithSnapshotSupport(sb, sb, c.k8s, namespace, c.log), nil
}

// GetSandbox re-attaches to an existing sandbox by claim name and wraps it
// with snapshot support.
func (c *PodSnapshotClient) GetSandbox(ctx context.Context, claimName, namespace string) (*SandboxWithSnapshotSupport, error) {
	sb, err := c.inner.GetSandbox(ctx, claimName, namespace)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "default"
	}
	return NewSandboxWithSnapshotSupport(sb, sb, c.k8s, namespace, c.log), nil
}

// DeleteSandbox closes the handle (if tracked) and deletes the underlying claim.
func (c *PodSnapshotClient) DeleteSandbox(ctx context.Context, claimName, namespace string) error {
	return c.inner.DeleteSandbox(ctx, claimName, namespace)
}

// DeleteAll closes and deletes all tracked sandboxes. Best-effort.
func (c *PodSnapshotClient) DeleteAll(ctx context.Context) {
	c.inner.DeleteAll(ctx)
}

// EnableAutoCleanup registers SIGINT/SIGTERM handlers to call DeleteAll.
// The returned stop function deregisters the handlers.
func (c *PodSnapshotClient) EnableAutoCleanup() func() {
	return c.inner.EnableAutoCleanup()
}

// checkSnapshotCRDInstalled uses the discovery API to verify that
// podsnapshot.gke.io/v1 is available on the cluster.
func checkSnapshotCRDInstalled(dc crdDiscoverer, log logr.Logger) error {
	groupVersion := PodSnapshotAPIGroup + "/" + PodSnapshotAPIVersion
	resources, err := dc.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if k8serrors.IsNotFound(err) || isGroupNotFound(err) {
			return fmt.Errorf("%w: %s API group not found on cluster", ErrCRDNotInstalled, groupVersion)
		}
		if k8serrors.IsForbidden(err) {
			// RBAC may prohibit discovery; assume the CRD exists and let
			// subsequent operations surface permission errors.
			log.Info("discovery check for PodSnapshot CRD returned 403; assuming CRD is installed", "warning", err.Error())
			return nil
		}
		return fmt.Errorf("snapshots: checking for PodSnapshot CRD: %w", err)
	}

	for _, r := range resources.APIResources {
		if r.Kind == "PodSnapshot" {
			return nil
		}
	}
	return fmt.Errorf("%w: PodSnapshot resource not found in %s", ErrCRDNotInstalled, groupVersion)
}

// isGroupNotFound returns true when the discovery client wraps a "group not found"
// error that is not surfaced as a standard k8s 404.
func isGroupNotFound(err error) bool {
	if err == nil {
		return false
	}
	// discovery.ErrGroupDiscoveryFailed wraps per-group errors.
	if derr, ok := err.(*discovery.ErrGroupDiscoveryFailed); ok {
		for _, gerr := range derr.Groups {
			if k8serrors.IsNotFound(gerr) {
				return true
			}
		}
	}
	return false
}
