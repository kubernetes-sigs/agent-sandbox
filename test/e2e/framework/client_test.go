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
	"testing"

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
