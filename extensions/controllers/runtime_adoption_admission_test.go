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

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
)

func TestRuntimeAdmissionRejectsOldAndUntrustedWriters(t *testing.T) {
	for _, name := range []string{
		"workload status", "old controller clears reservation", "old controller clears attempt", "replacement reservation", "cleared consumption",
		"removed finalizer", "owner before hold", "metadata before hold", "changed blueprint", "legacy assignment", "another status Sandbox",
		"pool-era claim readiness", "pool-era Sandbox readiness", "companion writes verification", "uncommitted runtime verification", "grant before metadata", "forged terminal result",
	} {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			f.reserve()
			var before, after client.Object = f.sandbox, f.sandbox.DeepCopy()
			username, subresource := testCompanionUsername, "status"
			switch name {
			case "workload status":
				username = "tenant"
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeAdoption = nil
			case "old controller clears reservation":
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeAdoption = nil
			case "old controller clears attempt":
				claim := f.claim.DeepCopy()
				claim.Status.RuntimeAdoption = nil
				before, after = f.claim, claim
			case "replacement reservation":
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeAdoption.Reservation.AttemptID = "replacement-attempt-0001"
			case "cleared consumption":
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeAdoption.Consumed = nil
			case "removed finalizer":
				subresource = ""
				after.SetFinalizers(nil)
			case "owner before hold":
				subresource = ""
				after.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(f.claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))})
			case "metadata before hold":
				subresource = ""
				after.SetLabels(map[string]string{"claim": "claim-a"})
			case "changed blueprint":
				subresource = ""
				after.(*sandboxv1beta1.Sandbox).Spec.PodTemplate.Spec.Containers[0].Image = "untrusted:latest"
			case "legacy assignment":
				subresource = ""
				claim := f.claim.DeepCopy()
				claim.Annotations = map[string]string{extensionsv1beta1.AssignedSandboxNameAnnotation: f.sandbox.Name}
				before, after = f.claim, claim
			case "another status Sandbox":
				claim := f.claim.DeepCopy()
				claim.Status.SandboxStatus.Name = "another-sandbox"
				before, after = f.claim, claim
			case "pool-era claim readiness":
				claim := f.claim.DeepCopy()
				claim.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				before, after = f.claim, claim
			case "pool-era Sandbox readiness":
				after.(*sandboxv1beta1.Sandbox).Status.Conditions[0].ObservedGeneration++
			case "companion writes verification":
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeActivationVerification = &sandboxv1beta1.RuntimeActivationVerification{}
			case "uncommitted runtime verification":
				username = testRuntimeUsername
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeActivationVerification = &sandboxv1beta1.RuntimeActivationVerification{Conditions: []metav1.Condition{{Type: "Verified", Status: metav1.ConditionTrue}}}
			case "grant before metadata":
				claim := f.claim.DeepCopy()
				claim.Status.RuntimeAdoption.Grant = &sandboxv1beta1.RuntimeAdoptionGrant{ExpiresAt: claim.Status.RuntimeAdoption.Reservation.ExpiresAt}
				before, after = f.claim, claim
			case "forged terminal result":
				after.(*sandboxv1beta1.Sandbox).Status.RuntimeAdoption.TerminalEvidenceDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			response := f.handler.Handle(f.ctx, protocolAdmissionRequest(t, before, after, username, subresource))
			require.False(t, response.Allowed, "admission accepted %s", name)
		})
	}
}

func TestRuntimeAdmissionProtectsPodSubresources(t *testing.T) {
	for _, name := range []string{"no-op", "ephemeralcontainers", "resize", "owner", "metadata", "delete", "eviction", "exec", "attach", "portforward", "pool-era readiness", "workload readiness"} {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			f.reserve()
			after := f.pod.DeepCopy()
			req := protocolAdmissionRequest(t, f.pod, after, testCompanionUsername, "")
			switch name {
			case "ephemeralcontainers":
				req.SubResource = name
				after.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "debug:latest"}}}
			case "resize":
				req.SubResource = name
				after.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
			case "owner":
				after.OwnerReferences = nil
			case "metadata":
				after.Labels["example.com/team"] = "unheld-target"
			case "delete":
				req.Operation = admissionv1.Delete
			case "eviction":
				req.Operation, req.SubResource = admissionv1.Create, "eviction"
			case "exec", "attach", "portforward":
				req.Operation, req.SubResource = admissionv1.Connect, name
			case "pool-era readiness", "workload readiness":
				req.SubResource = "status"
				if name == "pool-era readiness" {
					req.UserInfo.Username = testRuntimeUsername
				}
				after.Status.Conditions = []corev1.PodCondition{{Type: runtimeadoption.ActivationReadyCondition, Status: corev1.ConditionTrue}}
			}
			req.Object.Raw = protocolJSON(t, after)
			response := f.handler.Handle(f.ctx, req)
			require.Equal(t, name == "no-op", response.Allowed, "unexpected admission result for %s: %v", name, response.Result)
		})
	}
}

func TestRuntimeNodeEvidenceRequiresBoundLiveAgent(t *testing.T) {
	for _, name := range []string{"valid", "wrong service account", "missing bound token", "wrong node UID", "wrong Pod UID", "another node object", "replaced Pod"} {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeProtocolFixture(t)
			agent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime-agent", Namespace: testAgentNamespace, UID: "runtime-agent-uid"},
				Spec: corev1.PodSpec{NodeName: f.node.Node.Name, ServiceAccountName: "gatekeeper-runtime-agent"}}
			require.NoError(t, f.writer.Create(f.ctx, agent))
			status := &unstructured.Unstructured{}
			status.SetGroupVersionKind(runtimeadoption.NodeStatusGVK)
			require.NoError(t, f.writer.Get(f.ctx, client.ObjectKey{Name: f.node.Node.Name}, status))
			req := protocolAdmissionRequest(t, status, status, "system:serviceaccount:"+testAgentNamespace+":gatekeeper-runtime-agent", "status")
			req.UserInfo.Extra = map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {f.node.Node.Name}, "authentication.kubernetes.io/node-uid": {string(f.node.Node.UID)},
				"authentication.kubernetes.io/pod-name": {agent.Name}, "authentication.kubernetes.io/pod-uid": {string(agent.UID)},
			}
			switch name {
			case "wrong service account":
				req.UserInfo.Username = testCompanionUsername
			case "missing bound token":
				delete(req.UserInfo.Extra, "authentication.kubernetes.io/node-name")
			case "wrong node UID":
				req.UserInfo.Extra["authentication.kubernetes.io/node-uid"] = []string{"replacement-node"}
			case "wrong Pod UID":
				req.UserInfo.Extra["authentication.kubernetes.io/pod-uid"] = []string{"another-agent"}
			case "another node object":
				status.SetName("another-node")
				req.Object.Raw = protocolJSON(t, status)
			case "replaced Pod":
				require.NoError(t, f.writer.Delete(f.ctx, agent))
				agent.ResourceVersion, agent.UID = "", "replacement-agent"
				require.NoError(t, f.writer.Create(f.ctx, agent))
			}
			response := f.handler.Handle(f.ctx, req)
			require.Equal(t, name == "valid", response.Allowed, "unexpected admission result for %s: %v", name, response.Result)
		})
	}
}
