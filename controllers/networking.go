// Copyright 2025 The Kubernetes Authors.
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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

func (r *SandboxReconciler) reconcileNetworkingService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Service, error) {
	log := log.FromContext(ctx)

	// If no custom Service is specified, ensure any existing custom Service is deleted
	if sandbox.Spec.Networking.Service == nil {
		return nil, r.deleteNetworkingService(ctx, sandbox)
	}

	networkingServiceName := sandbox.Name + "-custom"
	service := &corev1.Service{}
	found := false

	if err := r.Get(ctx, types.NamespacedName{Name: networkingServiceName, Namespace: sandbox.Namespace}, service); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get custom Service")
			return nil, err
		}
	} else {
		found = true
	}

	// We should always override Selector
	sandbox.Spec.Networking.Service.Selector = map[string]string{
		sandboxLabel: nameHash,
	}
	if found {
		if reflect.DeepEqual(&service.Spec, sandbox.Spec.Networking.Service) {
			return service, nil
		}
		service.Spec = *sandbox.Spec.Networking.Service
		log.Info("Updating custom Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		if err := r.Update(ctx, service); err != nil {
			log.Error(err, "Failed to update custom Service")
			return nil, err
		}
		return service, nil
	}

	// Create new custom Service
	log.Info("Creating a new custom Service", "Service.Namespace", sandbox.Namespace, "Service.Name", networkingServiceName)
	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkingServiceName,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: nameHash,
			},
		},
		Spec: *sandbox.Spec.Networking.Service,
	}
	service.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(sandbox, service, r.Scheme); err != nil {
		return nil, err
	}

	if err := r.Create(ctx, service, client.FieldOwner("sandbox-controller")); err != nil {
		log.Error(err, "Failed to create custom Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return nil, err
	}
	return service, nil
}

func (r *SandboxReconciler) deleteNetworkingService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	log := log.FromContext(ctx)
	networkingServiceName := sandbox.Name + "-custom"
	service := &corev1.Service{}

	if err := r.Get(ctx, types.NamespacedName{Name: networkingServiceName, Namespace: sandbox.Namespace}, service); err != nil {
		if errors.IsNotFound(err) {
			// Service doesn't exist, nothing to delete
			return nil
		}
		return err
	}

	log.Info("Deleting custom Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
	if err := r.Delete(ctx, service); err != nil {
		log.Error(err, "Failed to delete custom Service")
		return err
	}
	return nil
}
