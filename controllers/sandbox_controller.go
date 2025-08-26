/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	"github.com/spotahome/kooper/v2/controller"
	"github.com/spotahome/kooper/v2/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/barney-s/agent-sandbox/api/v1alpha1"
)

const (
	// sandboxFinalizer is the finalizer for the sandbox controller.
	sandboxFinalizer = "agents.x-k8s.io/sandbox-finalizer"
)

// handler is the controller handler.
type sandboxHandler struct {
	client client.Client
	logger log.Logger
	// You can add here more clients that you need.
}

// NewSandboxHandler returns a new Sandbox handler.
func NewSandboxHandler(client client.Client, logger log.Logger) controller.Handler {
	h := &sandboxHandler{
		client: client,
		logger: logger,
	}
	return controller.HandlerFunc(h.Handle)
}

func (s *sandboxHandler) Handle(ctx context.Context, obj runtime.Object) error {
	sandbox, ok := obj.(*sandboxv1alpha1.Sandbox)
	if !ok {
		return fmt.Errorf("object is not a sandbox object")
	}
	s.logger.Infof("reconciling sandbox %s/%s", sandbox.Namespace, sandbox.Name)

	// Handle deletion.
	if !sandbox.ObjectMeta.DeletionTimestamp.IsZero() {
		return s.handleDelete(ctx, sandbox)
	}

	// Handle create/update.
	return s.handleCreateUpdate(ctx, sandbox)
}

func (s *sandboxHandler) handleCreateUpdate(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	// Add finalizer if not present.
	if !s.hasFinalizer(sandbox) {
		s.logger.Infof("adding finalizer to sandbox %s/%s", sandbox.Namespace, sandbox.Name)
		sandbox.ObjectMeta.Finalizers = append(sandbox.ObjectMeta.Finalizers, sandboxFinalizer)
		if err := s.client.Update(ctx, sandbox); err != nil {
			return fmt.Errorf("could not add finalizer: %w", err)
		}
	}

	// Create pod if not present.
	pod := &corev1.Pod{}
	err := s.client.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Name}, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create the pod.
			s.logger.Infof("creating pod for sandbox %s/%s", sandbox.Namespace, sandbox.Name)
			pod, err := s.newPod(sandbox)
			if err != nil {
				return fmt.Errorf("could not create pod: %w", err)
			}
			if err := s.client.Create(ctx, pod); err != nil {
				return fmt.Errorf("could not create pod: %w", err)
			}
		} else {
			return fmt.Errorf("could not get pod: %w", err)
		}
	}

	return nil
}

func (s *sandboxHandler) handleDelete(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	// If the finalizer is not present, we don't need to do anything.
	if !s.hasFinalizer(sandbox) {
		return nil
	}

	// Delete the pod.
	s.logger.Infof("deleting pod for sandbox %s/%s", sandbox.Namespace, sandbox.Name)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}
	err := s.client.Delete(ctx, pod)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("could not delete pod: %w", err)
	}

	// Remove finalizer.
	s.logger.Infof("removing finalizer from sandbox %s/%s", sandbox.Namespace, sandbox.Name)
	sandbox.ObjectMeta.Finalizers = s.removeFinalizer(sandbox.ObjectMeta.Finalizers)
	if err := s.client.Update(ctx, sandbox); err != nil {
		return fmt.Errorf("could not remove finalizer: %w", err)
	}

	return nil
}

func (s *sandboxHandler) hasFinalizer(sandbox *sandboxv1alpha1.Sandbox) bool {
	for _, f := range sandbox.ObjectMeta.Finalizers {
		if f == sandboxFinalizer {
			return true
		}
	}
	return false
}

func (s *sandboxHandler) removeFinalizer(finalizers []string) []string {
	var newFinalizers []string
	for _, f := range finalizers {
		if f != sandboxFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	return newFinalizers
}

func (s *sandboxHandler) newPod(sandbox *sandboxv1alpha1.Sandbox) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(sandbox, sandboxv1alpha1.SchemeBuilder.GroupVersion.WithKind("Sandbox")),
			},
		},
		Spec: sandbox.Spec.Template.Spec,
	}
	return pod, nil
}

// NewSandboxController returns a new Sandbox controller.
func NewSandboxController(cfg controller.Config, k8sclient kubernetes.Interface, crClient client.Client, logger log.Logger) (controller.Controller, error) {
	// Retriever.
	cfg.Retriever = controller.MustRetrieverFromListerWatcher(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				list := &sandboxv1alpha1.SandboxList{}
				err := crClient.List(context.Background(), list, &client.ListOptions{Raw: &options})
				return list, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return k8sclient.CoreV1().RESTClient().Get().
					Resource("sandboxes").
					VersionedParams(&options, metav1.ParameterCodec).
					Watch(context.Background())
			},
		},
	)

	// Handler.
	cfg.Handler = NewSandboxHandler(crClient, logger)
	cfg.Logger = logger

	// Controller.
	return controller.New(&cfg)
}
