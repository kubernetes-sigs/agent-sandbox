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

package runtimeadoption

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAdmissionRequiresUnfilteredSubresourceCoverage(t *testing.T) {
	configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: DefaultWebhookConfigurationName},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: AdmissionWebhookName, FailurePolicy: new(admissionregistrationv1.Fail), MatchPolicy: new(admissionregistrationv1.Equivalent),
			SideEffects: new(admissionregistrationv1.SideEffectClassNone), AdmissionReviewVersions: []string{"v1"}, Rules: AdmissionRules(),
			ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("test-ca"),
				Service: &admissionregistrationv1.ServiceReference{Name: "admission", Namespace: "agent-sandbox-system", Path: new(AdmissionPath)}},
		}}}
	scheme := runtime.NewScheme()
	require.NoError(t, admissionregistrationv1.AddToScheme(scheme))
	for _, name := range []string{"complete", "fail open", "namespace selector", "object selector", "match condition", "ephemeral containers", "resize", "eviction", "exec", "port forward", "claim status", "node status"} {
		t.Run(name, func(t *testing.T) {
			candidate := configuration.DeepCopy()
			webhook := &candidate.Webhooks[0]
			missing := ""
			switch name {
			case "fail open":
				webhook.FailurePolicy = new(admissionregistrationv1.Ignore)
			case "namespace selector":
				webhook.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"protected": "true"}}
			case "object selector":
				webhook.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"protected": "true"}}
			case "match condition":
				webhook.MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "skip", Expression: "false"}}
			case "ephemeral containers":
				missing = "pods/ephemeralcontainers"
			case "resize":
				missing = "pods/resize"
			case "eviction":
				missing = "pods/eviction"
			case "exec":
				missing = "pods/exec"
			case "port forward":
				missing = "pods/portforward"
			case "claim status":
				missing = "sandboxclaims/status"
			case "node status":
				missing = "runtimepolicynodestatuses/status"
			}
			if missing != "" {
				for i := range webhook.Rules {
					webhook.Rules[i].Resources = slices.DeleteFunc(webhook.Rules[i].Resources, func(resource string) bool { return resource == missing })
				}
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(candidate).Build()
			err := AdmissionReady(context.Background(), reader, candidate.Name)
			if name == "complete" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
