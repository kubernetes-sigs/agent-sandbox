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
	"errors"
	"fmt"
	"slices"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AdmissionPath                   = "/validate-runtime-adoption"
	AdmissionWebhookName            = "runtime-adoption.agents.x-k8s.io"
	DefaultWebhookConfigurationName = "agent-sandbox-runtime-adoption"
	ActivationReadyCondition        = "runtime.gatekeeper.sh/ActivationReady"
)

// AdmissionRules includes subresources that can mutate or inject work into the
// retained execution. Selectors must not narrow these rules based on mutable
// workload labels; the handler determines whether an object is protected.
func AdmissionRules() []admissionregistrationv1.RuleWithOperations {
	all := []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete, admissionregistrationv1.Connect}
	return []admissionregistrationv1.RuleWithOperations{
		{Operations: all, Rule: admissionregistrationv1.Rule{APIGroups: []string{"agents.x-k8s.io"}, APIVersions: []string{"v1beta1"}, Resources: []string{"sandboxes", "sandboxes/status"}}},
		{Operations: all, Rule: admissionregistrationv1.Rule{APIGroups: []string{"extensions.agents.x-k8s.io"}, APIVersions: []string{"v1beta1"}, Resources: []string{"sandboxclaims", "sandboxclaims/status"}}},
		{Operations: all, Rule: admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods", "pods/status", "pods/ephemeralcontainers", "pods/resize", "pods/eviction", "pods/exec", "pods/attach", "pods/portforward"}}},
		{Operations: all, Rule: admissionregistrationv1.Rule{APIGroups: []string{"runtime.gatekeeper.sh"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"runtimepolicynodestatuses", "runtimepolicynodestatuses/status"}}},
	}
}

// AdmissionReady checks registration, not a runtime capability. A successful
// protected status write also has to pass the fail-closed admission request.
// Only platform administrators may change this registration or its TLS secret.
func AdmissionReady(ctx context.Context, reader client.Reader, name string) error {
	if name == "" {
		return errors.New("runtime adoption admission registration is not configured")
	}
	var configuration admissionregistrationv1.ValidatingWebhookConfiguration
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, &configuration); err != nil {
		return fmt.Errorf("read runtime adoption admission registration: %w", err)
	}
	if configuration.DeletionTimestamp != nil {
		return errors.New("runtime adoption admission registration is terminating")
	}
	for _, webhook := range configuration.Webhooks {
		if webhook.Name != AdmissionWebhookName {
			continue
		}
		if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Fail ||
			webhook.SideEffects == nil || *webhook.SideEffects != admissionregistrationv1.SideEffectClassNone ||
			webhook.MatchPolicy == nil || *webhook.MatchPolicy != admissionregistrationv1.Equivalent ||
			webhook.ClientConfig.Service == nil || webhook.ClientConfig.Service.Path == nil || *webhook.ClientConfig.Service.Path != AdmissionPath ||
			len(webhook.ClientConfig.CABundle) == 0 || webhook.ClientConfig.URL != nil ||
			webhook.NamespaceSelector != nil && (len(webhook.NamespaceSelector.MatchLabels) != 0 || len(webhook.NamespaceSelector.MatchExpressions) != 0) ||
			webhook.ObjectSelector != nil && (len(webhook.ObjectSelector.MatchLabels) != 0 || len(webhook.ObjectSelector.MatchExpressions) != 0) ||
			len(webhook.MatchConditions) != 0 || !slices.Contains(webhook.AdmissionReviewVersions, "v1") {
			return errors.New("runtime adoption admission must be fail-closed with unfiltered resource coverage")
		}
		for _, required := range AdmissionRules() {
			for _, resource := range required.Resources {
				for _, operation := range required.Operations {
					covered := slices.ContainsFunc(webhook.Rules, func(rule admissionregistrationv1.RuleWithOperations) bool {
						return (rule.Scope == nil || *rule.Scope == admissionregistrationv1.AllScopes) &&
							slices.Contains(rule.APIGroups, required.APIGroups[0]) && slices.Contains(rule.APIVersions, required.APIVersions[0]) &&
							slices.Contains(rule.Resources, resource) && (slices.Contains(rule.Operations, operation) || slices.Contains(rule.Operations, admissionregistrationv1.OperationAll))
					})
					if !covered {
						return fmt.Errorf("runtime adoption admission does not protect %s %s/%s", operation, required.APIGroups[0], resource)
					}
				}
			}
		}
		return nil
	}
	return errors.New("runtime adoption admission webhook is missing")
}
