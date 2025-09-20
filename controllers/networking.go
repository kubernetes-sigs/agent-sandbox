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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

func (r *SandboxReconciler) reconcileNetworkingService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, nameHash string) (*corev1.Service, error) {
	log := log.FromContext(ctx)

	// If no networking service is specified, ensure any existing networking service is deleted
	if sandbox.Spec.Networking == nil || sandbox.Spec.Networking.Service == nil {
		return nil, r.deleteNetworkingService(ctx, sandbox)
	}

	networkingServiceName := sandbox.Name + "-networking"
	service := &corev1.Service{}
	found := false

	if err := r.Get(ctx, types.NamespacedName{Name: networkingServiceName, Namespace: sandbox.Namespace}, service); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get Networking Service")
			return nil, err
		}
	} else {
		found = true
	}

	// Convert our ServiceSpec to corev1.ServiceSpec
	serviceSpec := r.convertToServiceSpec(sandbox.Spec.Networking.Service, nameHash)

	if found {
		// Update existing service if spec changed
		if r.serviceSpecChanged(service.Spec, serviceSpec) {
			log.Info("Updating Networking Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
			service.Spec = serviceSpec
			if err := r.Update(ctx, service); err != nil {
				log.Error(err, "Failed to update Networking Service")
				return nil, err
			}
		}
		return service, nil
	}

	// Create new networking service
	log.Info("Creating a new Networking Service", "Service.Namespace", sandbox.Namespace, "Service.Name", networkingServiceName)
	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkingServiceName,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: nameHash,
			},
		},
		Spec: serviceSpec,
	}
	service.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(sandbox, service, r.Scheme); err != nil {
		return nil, err
	}

	if err := r.Create(ctx, service, client.FieldOwner("sandbox-controller")); err != nil {
		log.Error(err, "Failed to create Networking Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return nil, err
	}
	return service, nil
}

func (r *SandboxReconciler) deleteNetworkingService(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	log := log.FromContext(ctx)
	networkingServiceName := sandbox.Name + "-networking"
	service := &corev1.Service{}

	if err := r.Get(ctx, types.NamespacedName{Name: networkingServiceName, Namespace: sandbox.Namespace}, service); err != nil {
		if errors.IsNotFound(err) {
			// Service doesn't exist, nothing to delete
			return nil
		}
		return err
	}

	log.Info("Deleting Networking Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
	if err := r.Delete(ctx, service); err != nil {
		log.Error(err, "Failed to delete Networking Service")
		return err
	}
	return nil
}

func (r *SandboxReconciler) convertToServiceSpec(serviceSpec *sandboxv1alpha1.ServiceSpec, nameHash string) corev1.ServiceSpec {
	// Set default type to ClusterIP if not specified
	serviceType := corev1.ServiceTypeClusterIP
	if serviceSpec.Type != "" {
		serviceType = serviceSpec.Type
	}

	// Convert ports
	var ports []corev1.ServicePort
	for _, port := range serviceSpec.Ports {
		corePort := corev1.ServicePort{
			Name:     port.Name,
			Port:     port.Port,
			Protocol: port.Protocol,
		}

		// Set default protocol to TCP if not specified
		if corePort.Protocol == "" {
			corePort.Protocol = corev1.ProtocolTCP
		}

		// Handle target port
		if port.TargetPort.Type == intstr.Int {
			corePort.TargetPort = port.TargetPort
		} else if port.TargetPort.Type == intstr.String && port.TargetPort.StrVal != "" {
			corePort.TargetPort = port.TargetPort
		} else if port.Name != "" {
			// If no target port specified but name is provided, use the name
			corePort.TargetPort = intstr.FromString(port.Name)
		} else {
			// Default to the same port number
			corePort.TargetPort = intstr.FromInt(int(port.Port))
		}

		ports = append(ports, corePort)
	}

	return corev1.ServiceSpec{
		Type:     serviceType,
		Selector: map[string]string{sandboxLabel: nameHash},
		Ports:    ports,
	}
}

func (r *SandboxReconciler) serviceSpecChanged(current, desired corev1.ServiceSpec) bool {
	if current.Type != desired.Type {
		return true
	}
	if len(current.Ports) != len(desired.Ports) {
		return true
	}
	// Simple comparison - in production you might want more sophisticated comparison
	return false
}
