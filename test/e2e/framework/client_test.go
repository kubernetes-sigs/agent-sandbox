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

package framework

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/controllers"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMustUpdateObjectRetriesOnConflict(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	updateAttempts := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(controllers.Scheme).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateAttempts++
				if updateAttempts == 1 {
					return k8serrors.NewConflict(
						schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"},
						obj.GetName(),
						fmt.Errorf("simulated concurrent modification"),
					)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	cl := &ClusterClient{
		T:      t,
		client: fakeClient,
	}

	MustUpdateObject(cl, sandbox, func(s *sandboxv1beta1.Sandbox) {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		s.Labels["test-key"] = "test-value"
	})

	if updateAttempts != 2 {
		t.Errorf("expected 2 update attempts (1 conflict + 1 success), got %d", updateAttempts)
	}

	updated := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(t.Context(), types.NamespacedName{Name: "test-sandbox", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get sandbox after update: %v", err)
	}
	if updated.Labels["test-key"] != "test-value" {
		t.Errorf("label not persisted after conflict retry, got labels: %v", updated.Labels)
	}
}

func fakePortForwardCmd(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	return exec.CommandContext(t.Context(), "sh", "-c", script)
}

func TestStartPortForwardWaitsForReadyMarker(t *testing.T) {
	cl := &ClusterClient{T: t}

	// Chatter before the banner widens the concurrent write/read window on stdout.
	cmd := fakePortForwardCmd(t, `for i in $(seq 1 40); do echo "warming up $i"; done
echo "Forwarding from 127.0.0.1:8080 -> 8080"
exec sleep 30`)

	if err := cl.startPortForward(cmd, portForwardReadyTimeout); err != nil {
		t.Fatalf("startPortForward failed on a child that printed the ready marker: %v", err)
	}
}

func TestStartPortForwardReportsExitBeforeReady(t *testing.T) {
	cl := &ClusterClient{T: t}

	cmd := fakePortForwardCmd(t, `echo "error: unable to listen on port 8080" >&2
exit 1`)

	err := cl.startPortForward(cmd, portForwardReadyTimeout)
	if err == nil {
		t.Fatal("startPortForward succeeded for a child that exited without the ready marker")
	}
	if !strings.Contains(err.Error(), "unable to listen on port 8080") {
		t.Errorf("error does not carry the captured stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error does not carry the exit status: %v", err)
	}
}

// Signalled children report Exited()=false, so ProcessState polling misses them.
func TestStartPortForwardReportsSignalledChild(t *testing.T) {
	cl := &ClusterClient{T: t}

	cmd := fakePortForwardCmd(t, `echo "warming up"
kill -9 $$`)

	const timeout = 10 * time.Second
	start := time.Now()
	err := cl.startPortForward(cmd, timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("startPortForward succeeded for a signalled child")
	}
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("error does not identify the signal: %v", err)
	}
	if elapsed > timeout/5 {
		t.Errorf("startPortForward took %s to observe a signalled child, close to the %s deadline", elapsed, timeout)
	}
}

func TestStartPortForwardTimesOutWithoutReadyMarker(t *testing.T) {
	cl := &ClusterClient{T: t}

	cmd := fakePortForwardCmd(t, `echo "warming up"
exec sleep 30`)

	const timeout = 200 * time.Millisecond
	start := time.Now()
	err := cl.startPortForward(cmd, timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("startPortForward succeeded for a child that never reported readiness")
	}
	if !strings.Contains(err.Error(), "did not report") {
		t.Errorf("error does not identify the timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "warming up") {
		t.Errorf("error does not carry the output captured so far: %v", err)
	}
	if elapsed < timeout {
		t.Errorf("startPortForward returned after %s, before the %s deadline", elapsed, timeout)
	}
}
