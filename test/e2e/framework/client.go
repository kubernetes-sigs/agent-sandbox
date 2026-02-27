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

package framework

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	sandboxextensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultTimeout is the default timeout for WaitForObject and WaitForObjectNotFound.
	DefaultTimeout = 60 * time.Second
)

// ClusterClient is an abstraction layer for test cases to interact with the cluster.
type ClusterClient struct {
	T
	client        client.Client
	dynamicClient dynamic.Interface
	scheme        *runtime.Scheme
	watchSet      *WatchSet
}

// WatchSet is a shared set of watches for the ClusterClient to use.
func (cl *ClusterClient) WatchSet() *WatchSet {
	return cl.watchSet
}

// List retrieves a list of objects matching the provided options.
func (cl *ClusterClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	cl.Helper()
	if err := cl.client.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("list %T: %w", list, err)
	}
	return nil
}

// Delete deletes the specified object from the cluster.
func (cl *ClusterClient) Delete(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Deleting object %T (%s)", obj, nn.String())
	if err := cl.client.Delete(ctx, obj); err != nil {
		return fmt.Errorf("delete %T (%s): %w", obj, nn.String(), err)
	}
	return nil
}

// Get retrieves an object from the cluster.
func (cl *ClusterClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	cl.Helper()
	if err := cl.client.Get(ctx, key, obj); err != nil {
		return fmt.Errorf("get %T (%s): %w", obj, key.String(), err)
	}
	return nil
}

// Update an object that already exists on the cluster.
func (cl *ClusterClient) Update(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Updating object %T (%s)", obj, nn.String())
	if err := cl.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("update %T (%s): %w", obj, nn.String(), err)
	}
	return nil
}

// MustUpdateObject updates the specified object, using the provided updateFunc to modify
// the object.  This ensures that we always update the latest version of the object from the cluster.
// In future we might support automatic retries on optimistic-concurrency "misses".
func MustUpdateObject[T client.Object](cl *ClusterClient, obj T, updateFunc func(obj T)) {
	cl.Helper()

	ctx := cl.Context()

	id := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	latest := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(T)
	if err := cl.Get(ctx, id, latest); err != nil {
		cl.Fatalf("MustUpdateObject: failed to get latest %T (%s): %v", obj, id.String(), err)
	}

	updateFunc(latest)

	if err := cl.Update(ctx, latest); err != nil {
		cl.Fatalf("MustUpdateObject: failed to update %T (%s): %v", obj, id.String(), err)
	}
}

// CreateWithCleanup creates the specified object and cleans up the object after
// the test completes.
func (cl *ClusterClient) CreateWithCleanup(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Creating object %T (%s)", obj, nn.String())
	if err := cl.client.Create(ctx, obj); err != nil {
		return fmt.Errorf("CreateWithCleanup %T (%s): %w", obj, nn.String(), err)
	}
	cl.Cleanup(func() {
		cl.Helper()
		cl.Logf("Deleting object %T (%s)", obj, nn.String())
		// Use context.Background because test context is done during cleanup
		if err := cl.client.Delete(context.Background(), obj); err != nil && !k8serrors.IsNotFound(err) {
			cl.Errorf("CreateWithCleanup %T (%s): %s", obj, nn.String(), err)
		}
		if err := cl.WaitForObjectNotFound(context.Background(), obj); err != nil {
			cl.Errorf("CreateWithCleanup %T (%s): %s", obj, nn.String(), err)
		}
	})
	return nil
}

// Create creates the specified object in the cluster.
func (cl *ClusterClient) Create(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("Creating object %T (%s)", obj, nn.String())
	if err := cl.client.Create(ctx, obj); err != nil {
		return fmt.Errorf("create %T (%s): %w", obj, nn.String(), err)
	}
	return nil
}

// MustCreateWithCleanup is a wrapper around CreateWithCleanup that fails the test on error.
func (cl *ClusterClient) MustCreateWithCleanup(obj client.Object) {
	cl.Helper()
	ctx := cl.Context()

	if err := cl.CreateWithCleanup(ctx, obj); err != nil {
		cl.Fatalf("MustCreateWithCleanup(%T) failed with: %v", obj, err)
	}
}

// MatchesPredicates verifies the specified object exists and satisfies the provided
// predicates.
func (cl *ClusterClient) MatchesPredicates(ctx context.Context, obj client.Object, p ...predicates.ObjectPredicate) (bool, error) {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("MatchesPredicates %T (%s)", obj, nn.String())
	if err := cl.client.Get(ctx, nn, obj); err != nil {
		return false, fmt.Errorf("MatchesPredicates %T (%s): %w", obj, nn.String(), err)
	}
	for _, predicate := range p {
		predicateMatches, err := predicate.Matches(obj)
		if err != nil {
			return false, fmt.Errorf("MatchesPredicates %T (%s): %w", obj, nn.String(), err)
		}
		if !predicateMatches {
			return false, nil
		}
	}
	return true, nil
}

// MustMatchPredicates is a wrapper around MatchesPredicates that fails the test if the object
// does not exist, if the predicates are not satisfied or if there is an error during evaluation.
func (cl *ClusterClient) MustMatchPredicates(obj client.Object, p ...predicates.ObjectPredicate) {
	cl.Helper()
	ctx := cl.Context()

	matchesPredicates, err := cl.MatchesPredicates(ctx, obj, p...)
	if err != nil {
		cl.Fatalf("MustMatchPredicates(%T) failed with: %v", obj, err)
	}
	if !matchesPredicates {
		cl.Fatalf("MustMatchPredicates(%T) predicates not satisfied", obj)
	}
}

// MustExist fails the test if the object does not exist.
func (cl *ClusterClient) MustExist(obj client.Object) {
	cl.Helper()
	ctx := cl.Context()

	// We call MatchesPredicates without any predicates to just check for existence
	matchesPredicates, err := cl.MatchesPredicates(ctx, obj)
	if err != nil {
		cl.Fatalf("MustExist(%T) failed with: %v", obj, err)
	}
	if !matchesPredicates {
		cl.Fatalf("MustExist(%T) object does not exist", obj)
	}
}

// ValidateObjectNotFound verifies the specified object does not exist.
func (cl *ClusterClient) ValidateObjectNotFound(ctx context.Context, obj client.Object) error {
	cl.Helper()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	cl.Logf("ValidateObjectNotFound %T (%s)", obj, nn.String())
	err := cl.client.Get(ctx, nn, obj)
	if err == nil { // object still exists - error
		return fmt.Errorf("ValidateObjectNotFound %T (%s): object still exists",
			obj, nn.String())
	} else if !k8serrors.IsNotFound(err) { // unexpected error
		return fmt.Errorf("ValidateObjectNotFound %T (%s): %w",
			obj, nn.String(), err)
	}
	return nil // happy path - object not found
}

// PollUntilObjectMatches polls for the specified object to exist and satisfy the provided
// predicates. Use WaitForObject for more precise timing via watches.
func (cl *ClusterClient) PollUntilObjectMatches(obj client.Object, p ...predicates.ObjectPredicate) error {
	cl.Helper()
	ctx := cl.Context()

	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	start := time.Now()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	for {
		select {
		case <-ctx.Done():
			cl.Logf("Timed out waiting for object %s/%s", obj.GetNamespace(), obj.GetName())
			return fmt.Errorf("timed out waiting for object %s/%s", obj.GetNamespace(), obj.GetName())
		default:
			matchesPredicates, validationErr := cl.MatchesPredicates(ctx, obj, p...)
			if validationErr != nil {
				return validationErr
			}
			if matchesPredicates {
				cl.Logf("PollUntilObject %T (%s) took %s", obj, nn, time.Since(start))
				return nil
			}
			// Simple sleep for fixed duration (basic MVP)
			time.Sleep(time.Second)
		}
	}
}

// WaitForObject waits for the specified object to exist and satisfy
// the provided predicates.
// It will wait for the object to be created, but if the object is deleted,
// it will return an error.
// A timeout can be specified via the context, or it will default to 1 minute.
// It uses a watch for more precise timing than polling.
// It uses a shared WatchSet to avoid per-call watch setup latency.
func (cl *ClusterClient) WaitForObject(ctx context.Context, obj client.Object, p ...predicates.ObjectPredicate) error {
	cl.Helper()

	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	start := time.Now()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}

	// First check if the object already satisfies the predicates
	if valid, validationErr := cl.MatchesPredicates(ctx, obj, p...); validationErr == nil && valid {
		return nil
	}

	gvk, err := gvkForObject(obj)
	if err != nil {
		cl.Fatalf("failed to get GVK: %v", err)
	}

	gvr, err := gvrForGVK(gvk)
	if err != nil {
		cl.Fatalf("failed to get GVR for GVK %v: %v", gvk, err)
	}

	watchFilter := WatchFilter{Namespace: obj.GetNamespace(), Name: obj.GetName()}

	var lastNotMatching []predicates.ObjectPredicate
	done, err := Watch(cl, gvr, watchFilter, func(event watch.Event, obj *unstructured.Unstructured) (bool, error) {
		if event.Type == watch.Deleted {
			return false, fmt.Errorf("object was deleted while waiting for predicates to be satisfied")
		}

		// Check if predicates are satisfied
		var notMatching []predicates.ObjectPredicate
		for _, predicate := range p {
			if match, err := predicate.Matches(obj); err != nil {
				return false, err
			} else if !match {
				notMatching = append(notMatching, predicate)
			}
		}

		lastNotMatching = notMatching
		return len(notMatching) == 0, nil
	})
	if err != nil {
		return err
	}
	if !done {
		// Predicates not satisfied within timeout
		return fmt.Errorf("object did not satisfy predicates: %v", lastNotMatching)
	}
	cl.Logf("WaitForObject %T (%s) took %s", obj, nn, time.Since(start))
	return nil
}

// Watch calls a callback whenever the specified object changes,
// using a shared WatchSet to avoid per-call watch setup latency.
// Callback is called for each event, and if the callback returns true or an error, the watch will stop and the value will be returned.
func (cl *ClusterClient) Watch(gvr schema.GroupVersionResource, filter WatchFilter, callback func(event watch.Event) (bool, error)) (bool, error) {
	ctx := cl.Context()

	// Subscribe using the watchSet, ideally reusing an existing watch
	sub := cl.watchSet.Subscribe(ctx, gvr, filter)
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("timed out watching object: %w", ctx.Err())

		case event, ok := <-sub.Events:
			if !ok {
				return false, fmt.Errorf("subscription closed during watch of %v", gvr)
			}

			if event.Type == watch.Error {
				return false, fmt.Errorf("received error event during watch of %v", gvr)
			}

			if done, err := callback(event); done || err != nil {
				return done, err
			}
		}
	}
}

// Watch calls a callback whenever the specified object changes,
// using a shared WatchSet to avoid per-call watch setup latency.
// It is a wrapper around ClusterClient Watch that converts to a strongly-typed object in the callback.
func Watch[T client.Object](cl *ClusterClient, gvr schema.GroupVersionResource, watchFilter WatchFilter, callback func(event watch.Event, obj T) (bool, error)) (bool, error) {
	// Subscribe using the watchSet, ideally reusing an existing watch
	return cl.Watch(gvr, watchFilter, func(event watch.Event) (bool, error) {
		switch event.Type {
		case watch.Added, watch.Modified, watch.Deleted:
		// ok
		case watch.Bookmark:
			// Ignore
			return false, nil
		default:
			return false, fmt.Errorf("unexpected watch event type: %v", event.Type)
		}

		u, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			return false, fmt.Errorf("unexpected type for event object: %T", event.Object)
		}

		var t T

		switch any(t).(type) {
		case *unstructured.Unstructured:
			return callback(event, any(u).(T))
		default:
			// Copy the unstructured data to the provided object
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &t); err != nil {
				return false, fmt.Errorf("failed to convert unstructured to object: %w", err)
			}

			return callback(event, t)
		}
	})
}

// MustWatch is a wrapper around WatchObject that fails the test on error.
func MustWatch[T client.Object](cl *ClusterClient, gvr schema.GroupVersionResource, watchFilter WatchFilter, callback func(event watch.Event, obj T) (bool, error)) bool {
	cl.Helper()

	done, err := Watch(cl, gvr, watchFilter, callback)
	if err != nil {
		// Only fail the test if this isn't the normal "context cancelled" shutdown error.
		if !errors.Is(err, context.Canceled) {
			cl.Fatalf("Watch(%v, %+v) failed with: %v", gvr, watchFilter, err)
		}
	}
	return done
}

// gvkForObject returns the GroupVersionKind for the given object.
func gvkForObject(obj runtime.Object) (schema.GroupVersionKind, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" {
		return gvk, nil
	}

	switch o := obj.(type) {
	case *corev1.Pod:
		return schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, nil
	case *sandboxv1alpha1.Sandbox:
		return sandboxGVK, nil
	case *sandboxextensionsv1alpha1.SandboxWarmPool:
		return sandboxWarmpoolGVK, nil
	case *sandboxextensionsv1alpha1.SandboxClaim:
		return sandboxClaimGVK, nil
	default:
		return schema.GroupVersionKind{}, fmt.Errorf("no GVK found for object type %T", o)
	}
}

// gvrForGVK returns the GroupVersionResource for the given GroupVersionKind.
func gvrForGVK(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	// We use a hard-coded list rather than going through discovery for simplicity and speed.
	gv := gvk.GroupVersion()
	switch gvk.GroupKind() {
	case sandboxGVK.GroupKind():
		return gv.WithResource("sandboxes"), nil
	case sandboxWarmpoolGVK.GroupKind():
		return gv.WithResource("sandboxwarmpools"), nil
	case schema.GroupKind{Kind: "Pod"}:
		return gv.WithResource("pods"), nil
	case schema.GroupKind{Kind: "Namespace"}:
		return gv.WithResource("namespaces"), nil
	case schema.GroupKind{Kind: "SandboxClaim", Group: "extensions.agents.x-k8s.io"}:
		return gv.WithResource("sandboxclaims"), nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unknown GVK %v in gvrForGVK", gvk)
	}
}

// MustWaitForObject is a wrapper around WaitForObject that fails the test on error.
func (cl *ClusterClient) MustWaitForObject(obj client.Object, p ...predicates.ObjectPredicate) {
	cl.Helper()
	ctx := cl.Context()

	if err := cl.WaitForObject(ctx, obj, p...); err != nil {
		cl.Fatalf("MustWaitForObject(%T, %v) failed with: %v", obj, p, err)
	}
}

// WaitForObjectNotFound waits for the specified object to not exist.
func (cl *ClusterClient) WaitForObjectNotFound(ctx context.Context, obj client.Object) error {
	cl.Helper()
	// Static 1 minute timeout, this can be adjusted if needed
	timeoutCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)

	defer cancel()
	start := time.Now()
	nn := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	defer func() {
		cl.Helper()
		cl.Logf("WaitForObjectNotFound %T (%s) took %s", obj, nn, time.Since(start))
	}()
	var validationErr error
	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timed out waiting for object: %w", validationErr)
		default:
			if validationErr = cl.ValidateObjectNotFound(timeoutCtx, obj); validationErr == nil {
				return nil
			}
			// Simple sleep for fixed duration (basic MVP)
			time.Sleep(time.Second)
		}
	}
}

// validateAgentSandboxInstallation verifies agent-sandbox system components are
// installed.
func (cl *ClusterClient) validateAgentSandboxInstallation() error {
	cl.Helper()
	// verify CRDs exist
	crds := []string{
		"sandboxes.agents.x-k8s.io",
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxtemplates.extensions.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}
	for _, name := range crds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = name
		cl.MustExist(crd)
	}
	// verify agent-sandbox-system namespace exists
	ns := &corev1.Namespace{}
	ns.Name = "agent-sandbox-system"
	cl.MustExist(ns)
	// verify agent-sandbox-controller exists
	ctrlNN := types.NamespacedName{
		Name:      "agent-sandbox-controller",
		Namespace: ns.Name,
	}
	ctrl := &appsv1.Deployment{}
	ctrl.Name = ctrlNN.Name
	ctrl.Namespace = ctrlNN.Namespace
	cl.MustExist(ctrl)
	return nil
}

func (cl *ClusterClient) PortForward(ctx context.Context, pod types.NamespacedName, localPort, remotePort int) error {
	cl.Helper()
	// Set up a port-forward to the Chrome Debug Port
	portForward := exec.CommandContext(ctx, "kubectl", "-n", pod.Namespace,
		"port-forward", "pod/"+pod.Name, fmt.Sprintf("%d:%d", localPort, remotePort))
	cl.Logf("starting port-forward: %s", portForward.String())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	portForward.Stdout = io.MultiWriter(os.Stdout, &stdout)
	portForward.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := portForward.Start(); err != nil {
		return fmt.Errorf("failed to start port-forward: %w", err)
	}

	stopProcess := func() {
		if portForward.ProcessState != nil {
			if portForward.ProcessState.Exited() {
				return
			}
		}
		cl.Log("killing port-forward")
		if err := portForward.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			cl.Errorf("failed to kill port-forward: %s", err)
		}
	}
	cl.T.Cleanup(stopProcess)

	go func() {
		cl.Helper()
		if err := portForward.Wait(); err != nil {
			cl.Logf("port-forward exited with error: %s", err)
		} else {
			cl.Log("port-forward exited")
		}
	}()

	// There is a delay after starting the process before it starts listening.
	// Wait for the "Forwarding from" message
	for {
		if portForward.ProcessState != nil {
			if portForward.ProcessState.Exited() {
				return fmt.Errorf("port-forward process exited unexpectedly: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		}

		// Check stdout for the "Forwarding from" message
		if strings.Contains(stdout.String(), "Forwarding from") {
			cl.Logf("port-forward is ready\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

var sandboxGVK = schema.GroupVersionKind{
	Group:   "agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "Sandbox",
}

var sandboxWarmpoolGVK = schema.GroupVersionKind{
	Group:   "extensions.agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "SandboxWarmPool",
}

var sandboxClaimGVK = schema.GroupVersionKind{
	Group:   "extensions.agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    "SandboxClaim",
}

func (cl *ClusterClient) WaitForSandboxReady(ctx context.Context, sandboxID types.NamespacedName) error {
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	sandbox.SetName(sandboxID.Name)
	sandbox.SetNamespace(sandboxID.Namespace)
	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := cl.WaitForObject(timeoutCtx, sandbox, predicates.ReadyConditionIsTrue); err != nil {
		cl.Logf("waiting for sandbox to be ready: %v", err)
		return err
	}
	return nil
}

func (cl *ClusterClient) WaitForWarmPoolReady(ctx context.Context, sandboxWarmpoolID types.NamespacedName) error {
	cl.Helper()
	cl.Logf("Waiting for SandboxWarmPool Pods to be ready: warmpoolID - %s", sandboxWarmpoolID)

	warmpool := &unstructured.Unstructured{}
	warmpool.SetGroupVersionKind(sandboxWarmpoolGVK)
	if err := cl.client.Get(ctx, sandboxWarmpoolID, warmpool); err != nil {
		cl.T.Fatalf("Failed to get SandboxWarmPool %s: %v", sandboxWarmpoolID, err)
		return err
	}

	if err := cl.WaitForObject(ctx, warmpool, predicates.ReadyReplicasConditionIsTrue); err != nil {
		cl.T.Fatalf("waiting for warmpool to be ready: %v", err)
		return err
	}
	return nil

}

// GetSandbox returns the Sandbox object from the cluster.
func (cl *ClusterClient) GetSandbox(ctx context.Context, sandboxID types.NamespacedName) (*unstructured.Unstructured, error) {
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(sandboxGVK)
	sandbox.SetName(sandboxID.Name)
	sandbox.SetNamespace(sandboxID.Namespace)

	if err := cl.client.Get(ctx, sandboxID, sandbox); err != nil {
		cl.Logf("failed to get Sandbox %s: %v", sandboxID, err)
		return nil, err
	}
	return sandbox, nil
}

// ExecuteOnNode executes a command on a node.
// nodeName should be the Kubernetes node name (e.g., "agent-sandbox-control-plane").
// This function assumes we're running on kind, we'll need to adjust this (for example to use SSH) if we want to run on other platforms.
// For kind, it uses "docker exec" to run the command in the kind node container.
func (cl *ClusterClient) ExecuteOnNode(ctx context.Context, nodeName string, command []string) (string, string, error) {
	cl.Helper()

	args := append([]string{"exec", nodeName}, command...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cl.Logf("executing on kind node: docker %s", strings.Join(args, " "))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("docker exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), stderr.String(), nil
}
