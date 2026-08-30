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

package agtsbx

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
	"sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
)

func TestBuildSandbox(t *testing.T) {
	spec := Spec{
		Image:    "sandboxd:latest",
		Name:     "agtsbx-abc123",
		GRPCPort: 9090,
		RESTPort: 8080,
	}

	sb := buildSandbox(spec, "agents")

	t.Run("sets identity", func(t *testing.T) {
		assert.Equal(t, "agtsbx-abc123", sb.Name)
		assert.Equal(t, "agents", sb.Namespace)
		// Failed-create cleanup deletes only labelled objects.
		assert.Equal(t, managedByValue, sb.Labels[managedByLabel])
	})

	t.Run("runs the requested image in a single container", func(t *testing.T) {
		containers := sb.Spec.PodTemplate.Spec.Containers
		require.Len(t, containers, 1)
		assert.Equal(t, sandboxContainerName, containers[0].Name)
		assert.Equal(t, "sandboxd:latest", containers[0].Image)
	})

	t.Run("does not persist the command's environment", func(t *testing.T) {
		// The variables travel with the sandboxd request instead, so a
		// credential is not readable by anyone with get on sandboxes.
		assert.Empty(t, sb.Spec.PodTemplate.Spec.Containers[0].Env)
	})

	t.Run("exposes both sandboxd ports", func(t *testing.T) {
		ports := sb.Spec.PodTemplate.Spec.Containers[0].Ports
		require.Len(t, ports, 2)
		assert.Equal(t, int32(8080), ports[0].ContainerPort)
		assert.Equal(t, int32(9090), ports[1].ContainerPort)
	})

	t.Run("probes readiness on the sandboxd health endpoint", func(t *testing.T) {
		// Without it the Sandbox reports Ready as soon as the pod runs, and
		// the run fails on its first RPC instead of waiting.
		probe := sb.Spec.PodTemplate.Spec.Containers[0].ReadinessProbe
		require.NotNil(t, probe)
		require.NotNil(t, probe.HTTPGet)
		assert.Equal(t, "/v1/health", probe.HTTPGet.Path)
		assert.Equal(t, int32(8080), probe.HTTPGet.Port.IntVal)
	})

	t.Run("hardens the pod", func(t *testing.T) {
		// These match examples/sandboxd-sandbox, so a sandbox behaves the same
		// whether agtsbx created it or a manifest did.
		podSpec := sb.Spec.PodTemplate.Spec
		require.NotNil(t, podSpec.AutomountServiceAccountToken)
		assert.False(t, *podSpec.AutomountServiceAccountToken, "the sandbox must not receive an API token")

		require.NotNil(t, podSpec.SecurityContext)
		require.NotNil(t, podSpec.SecurityContext.RunAsNonRoot)
		assert.True(t, *podSpec.SecurityContext.RunAsNonRoot)

		container := podSpec.Containers[0]
		require.NotNil(t, container.SecurityContext)
		require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
		assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
		require.NotNil(t, container.SecurityContext.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
	})

	t.Run("honours custom ports", func(t *testing.T) {
		custom := buildSandbox(Spec{Image: "i", Name: "n", GRPCPort: 1234, RESTPort: 5678}, "default")
		container := custom.Spec.PodTemplate.Spec.Containers[0]
		assert.Equal(t, int32(5678), container.Ports[0].ContainerPort)
		assert.Equal(t, int32(1234), container.Ports[1].ContainerPort)
		assert.Equal(t, int32(5678), container.ReadinessProbe.HTTPGet.Port.IntVal)
	})
}

// newFakeKubeBackend builds a kubeBackend wired to a fake API server holding
// the given sandboxes.
//
// They are created through the typed client rather than pre-seeded into
// fake.NewSimpleClientset: the tracker derives the resource name with
// meta.UnsafeGuessKindToResource, which pluralizes "Sandbox" to "sandboxs", so
// a seeded object is filed where the client never looks and appears missing.
func newFakeKubeBackend(t *testing.T, readyTimeout time.Duration, objects ...*sandboxv1beta1.Sandbox) (*kubeBackend, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	for _, obj := range objects {
		_, err := client.AgentsV1beta1().Sandboxes(obj.Namespace).Create(t.Context(), obj, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return &kubeBackend{
		helper:       &sandbox.K8sHelper{AgentsClient: client.AgentsV1beta1()},
		namespace:    "default",
		readyTimeout: readyTimeout,
		progress:     io.Discard,
	}, client
}

// newSandbox builds a Sandbox carrying the given Ready condition status. An
// empty status means the Ready condition is absent entirely.
func newSandbox(ready metav1.ConditionStatus) *sandboxv1beta1.Sandbox {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
	}
	if ready != "" {
		sb.Status.Conditions = []metav1.Condition{
			{Type: string(sandboxv1beta1.SandboxConditionReady), Status: ready},
		}
	}
	return sb
}

func TestKubeBackendWaitReady(t *testing.T) {
	t.Run("returns immediately when the sandbox is already ready", func(t *testing.T) {
		// With a warm image the Sandbox can go Ready before a watch could be
		// established, so the current state must be checked first.
		backend, _ := newFakeKubeBackend(t, 5*time.Second, newSandbox(metav1.ConditionTrue))

		require.NoError(t, backend.waitReady(t.Context(), "box"))
	})

	t.Run("returns once a watch event reports ready", func(t *testing.T) {
		backend, client := newFakeKubeBackend(t, 10*time.Second, newSandbox(metav1.ConditionFalse))

		watcher := watch.NewFake()
		client.PrependWatchReactor("sandboxes", k8stesting.DefaultWatchReactor(watcher, nil))

		go func() {
			watcher.Modify(newSandbox(metav1.ConditionTrue))
		}()

		require.NoError(t, backend.waitReady(t.Context(), "box"))
	})

	t.Run("fails when the sandbox is deleted before becoming ready", func(t *testing.T) {
		// The object is gone and can never become ready.
		backend, client := newFakeKubeBackend(t, time.Minute, newSandbox(metav1.ConditionFalse))

		watcher := watch.NewFake()
		client.PrependWatchReactor("sandboxes", k8stesting.DefaultWatchReactor(watcher, nil))

		go func() {
			watcher.Delete(newSandbox(metav1.ConditionFalse))
		}()

		err := backend.waitReady(t.Context(), "box")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deleted before it became ready")
	})

	t.Run("reports a watch error instead of waiting out the timeout", func(t *testing.T) {
		backend, client := newFakeKubeBackend(t, time.Minute, newSandbox(metav1.ConditionFalse))

		watcher := watch.NewFake()
		client.PrependWatchReactor("sandboxes", k8stesting.DefaultWatchReactor(watcher, nil))

		go func() {
			watcher.Error(&metav1.Status{Message: "too old resource version"})
		}()

		err := backend.waitReady(t.Context(), "box")
		require.Error(t, err)
		// The real cause must survive: a bare timeout would hide it.
		assert.Contains(t, err.Error(), "too old resource version")
	})

	t.Run("times out when the sandbox never becomes ready", func(t *testing.T) {
		backend, client := newFakeKubeBackend(t, 200*time.Millisecond, newSandbox(metav1.ConditionFalse))

		watcher := watch.NewFake()
		client.PrependWatchReactor("sandboxes", k8stesting.DefaultWatchReactor(watcher, nil))

		err := backend.waitReady(t.Context(), "box")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not become ready")
	})

	t.Run("reports a missing sandbox", func(t *testing.T) {
		backend, _ := newFakeKubeBackend(t, 5*time.Second)

		err := backend.waitReady(t.Context(), "box")
		require.Error(t, err)
		assert.True(t, k8serrors.IsNotFound(errors.Unwrap(err)))
	})
}

func TestKubeInstanceStop(t *testing.T) {
	t.Run("deletes an ephemeral sandbox and stops the port-forward", func(t *testing.T) {
		backend, client := newFakeKubeBackend(t, time.Second, newSandbox(metav1.ConditionTrue))
		forwardStopped := false
		instance := &kubeInstance{
			backend:     backend,
			name:        "box",
			remove:      true,
			stopForward: func() { forwardStopped = true },
		}

		require.NoError(t, instance.Stop(t.Context()))

		assert.True(t, forwardStopped, "the port-forward must be torn down")
		_, err := client.AgentsV1beta1().Sandboxes("default").Get(t.Context(), "box", metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(err), "the sandbox should have been deleted")
	})

	t.Run("keeps the sandbox but still stops the port-forward", func(t *testing.T) {
		// --keep is about the sandbox, not local resources: leaking the
		// forwarder's goroutines would be a bug either way.
		backend, client := newFakeKubeBackend(t, time.Second, newSandbox(metav1.ConditionTrue))
		forwardStopped := false
		instance := &kubeInstance{
			backend:     backend,
			name:        "box",
			remove:      false,
			stopForward: func() { forwardStopped = true },
		}

		require.NoError(t, instance.Stop(t.Context()))

		assert.True(t, forwardStopped)
		_, err := client.AgentsV1beta1().Sandboxes("default").Get(t.Context(), "box", metav1.GetOptions{})
		require.NoError(t, err, "--keep must leave the sandbox in place")
	})

	t.Run("treats an already-deleted sandbox as success", func(t *testing.T) {
		// Deletion is the desired end state; losing the race to it is not an
		// error worth reporting.
		backend, _ := newFakeKubeBackend(t, time.Second)
		instance := &kubeInstance{backend: backend, name: "gone", remove: true, stopForward: func() {}}

		require.NoError(t, instance.Stop(t.Context()))
	})

	t.Run("deletes in the foreground so the pod goes with the sandbox", func(t *testing.T) {
		// Background garbage collection could leave the pod, and the
		// command's detached processes, running after Stop returned.
		backend, client := newFakeKubeBackend(t, time.Second, newSandbox(metav1.ConditionTrue))
		instance := &kubeInstance{backend: backend, name: "box", remove: true, stopForward: func() {}}

		require.NoError(t, instance.Stop(t.Context()))

		var policies []metav1.DeletionPropagation
		for _, action := range client.Actions() {
			if deletion, ok := action.(k8stesting.DeleteActionImpl); ok && deletion.GetDeleteOptions().PropagationPolicy != nil {
				policies = append(policies, *deletion.GetDeleteOptions().PropagationPolicy)
			}
		}
		assert.Equal(t, []metav1.DeletionPropagation{metav1.DeletePropagationForeground}, policies)
	})
}

func TestKubeBackendDeleteIfOurs(t *testing.T) {
	ourSandbox := func() *sandboxv1beta1.Sandbox {
		sb := newSandbox(metav1.ConditionTrue)
		sb.Labels = map[string]string{managedByLabel: managedByValue}
		return sb
	}

	t.Run("deletes a sandbox left behind by an ambiguous create", func(t *testing.T) {
		// The API server can persist the object and still fail the call.
		backend, client := newFakeKubeBackend(t, time.Second, ourSandbox())

		backend.deleteIfOurs("box")

		_, err := client.AgentsV1beta1().Sandboxes("default").Get(t.Context(), "box", metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(err))
	})

	t.Run("leaves a sandbox it did not create", func(t *testing.T) {
		backend, client := newFakeKubeBackend(t, time.Second, newSandbox(metav1.ConditionTrue))

		backend.deleteIfOurs("box")

		_, err := client.AgentsV1beta1().Sandboxes("default").Get(t.Context(), "box", metav1.GetOptions{})
		require.NoError(t, err)
	})
}

func TestSandboxReady(t *testing.T) {
	ready := func(status metav1.ConditionStatus) *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			Status: sandboxv1beta1.SandboxStatus{
				Conditions: []metav1.Condition{
					{Type: "PodScheduled", Status: metav1.ConditionTrue},
					{Type: string(sandboxv1beta1.SandboxConditionReady), Status: status},
				},
			},
		}
	}

	assert.True(t, sandboxReady(ready(metav1.ConditionTrue)))
	assert.False(t, sandboxReady(ready(metav1.ConditionFalse)))
	assert.False(t, sandboxReady(ready(metav1.ConditionUnknown)))

	// A Sandbox with no conditions has not been reconciled yet.
	assert.False(t, sandboxReady(&sandboxv1beta1.Sandbox{}))

	// Other conditions being True must not be mistaken for readiness.
	assert.False(t, sandboxReady(&sandboxv1beta1.Sandbox{
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{Type: "PodScheduled", Status: metav1.ConditionTrue}},
		},
	}))
}
