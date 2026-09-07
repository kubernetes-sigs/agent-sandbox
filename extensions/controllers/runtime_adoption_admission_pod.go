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
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/internal/runtimeadoption"
	"sigs.k8s.io/agent-sandbox/internal/utils"
)

func (h *RuntimeAdoptionAdmissionHandler) validatePodRequest(ctx context.Context, request admission.Request) error {
	if request.Operation == admissionv1.Connect || request.SubResource == "eviction" {
		var pod corev1.Pod
		if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Name}, &pod); err != nil {
			return client.IgnoreNotFound(err)
		}
		sandbox, err := h.sandboxForPod(ctx, &pod)
		if err != nil || sandbox == nil {
			return err
		}
		if request.SubResource == "eviction" {
			return validateRuntimePodDeletion(sandbox)
		}
		if status := sandbox.Status.RuntimeAdoption; status != nil && status.Reservation != nil {
			if status.CommitDigest == "" || status.TerminationRequestedTime != nil || status.TerminalEvidenceDigest != "" {
				return errors.New("external commands and connections cannot enter a pending or cancelling runtime adoption")
			}
			return nil
		}
		strict, err := strictRuntimeSandbox(ctx, h.Reader, sandbox)
		if err != nil {
			return err
		}
		if strict && utils.MatchesGroupKind(metav1.GetControllerOf(sandbox), extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) {
			return errors.New("external commands and connections cannot enter an unused strict pool execution")
		}
		return nil
	}
	var before, after corev1.Pod
	if err := h.decodeObjects(request, &before, &after); err != nil {
		return err
	}
	oldReady, newReady := activationReadyCondition(&before), activationReadyCondition(&after)
	if !reflect.DeepEqual(oldReady, newReady) && request.UserInfo.Username != h.RuntimeControllerUsername {
		return errors.New("runtime ActivationReady is owned by the runtime readiness controller")
	}
	lookup := &after
	if request.Operation != admissionv1.Create {
		lookup = &before
	}
	sandbox, err := h.sandboxForPod(ctx, lookup)
	if err != nil {
		return err
	}
	if sandbox == nil && request.Operation == admissionv1.Update {
		// An existing orphan cannot acquire strict pool provenance by adding a
		// Sandbox owner reference, even when an older core controller does it.
		sandbox, err = h.sandboxForPod(ctx, &after)
		if err != nil {
			return err
		}
		if sandbox != nil {
			strict, err := strictRuntimeSandbox(ctx, h.Reader, sandbox)
			if err != nil {
				return err
			}
			if strict {
				return errors.New("strict Sandbox Pods must have their owner established at cold creation")
			}
		}
	}
	if sandbox == nil {
		return nil
	}
	if request.Operation == admissionv1.Delete {
		return validateRuntimePodDeletion(sandbox)
	}
	status := sandbox.Status.RuntimeAdoption
	if status != nil && status.Reservation != nil {
		if request.Operation == admissionv1.Create {
			return errors.New("a reserved runtime Pod cannot be recreated")
		}
		claim, err := h.claimForSandbox(ctx, sandbox)
		if err != nil {
			return err
		}
		if claim.Status.RuntimeAdoption.PodRef.Name != after.Name || claim.Status.RuntimeAdoption.PodRef.UID != after.UID {
			return errors.New("pod update belongs to another runtime execution")
		}
		if !reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.OwnerReferences, after.OwnerReferences) {
			return errors.New("reserved Pod specification and owner are immutable, including ephemeral containers and resize")
		}
		if newReady != nil && newReady.Status == corev1.ConditionTrue && !reflect.DeepEqual(oldReady, newReady) &&
			(status.CommitDigest == "" || status.CommitDigest != claim.Status.RuntimeAdoption.CommitDigest ||
				status.TerminationRequestedTime != nil || claim.Status.RuntimeAdoption.TerminationRequestedTime != nil) {
			return errors.New("runtime readiness requires the current claim's confirmed commit")
		}
		if !sameAdmissionMetadata(&before, &after) {
			if request.UserInfo.Username != h.ControllerUsername || status.HoldEvidenceDigest == "" || status.TargetMetadataDigest != "" ||
				status.CommitDigest != "" || status.TerminationRequestedTime != nil || !metav1.IsControlledBy(sandbox, claim) {
				return errors.New("reserved Pod metadata may change only during the held target transition")
			}
			attempt, err := h.readAdmissionAttempt(ctx, claim, sandbox)
			if err != nil {
				return err
			}
			if err := h.currentIntent(claim, sandbox, attempt.namespace); err != nil {
				return err
			}
			_, holdDigest, err := attempt.evidence.VerifyHold(attempt.observation, status.Reservation, status.Initialization, attempt.pod, true)
			if err != nil || holdDigest != status.HoldEvidenceDigest {
				return errors.Join(errors.New("pod metadata has no matching current hold"), err)
			}
			expected := sandboxcontrollers.ExpectedSandboxPodMetadata(ctx, sandbox, &before)
			if !sameAdmissionMetadata(&expected, &after) {
				return errors.New("pod metadata differs from the exact held target transformation")
			}
		}
		return nil
	}
	strict, err := strictRuntimeSandbox(ctx, h.Reader, sandbox)
	if err != nil {
		return err
	}
	if !strict {
		return nil
	}
	if request.Operation == admissionv1.Create && request.UserInfo.Username != h.ControllerUsername {
		return errors.New("strict Sandbox origin requires a controller-created Pod")
	}
	if request.Operation == admissionv1.Update && !reflect.DeepEqual(before.OwnerReferences, after.OwnerReferences) {
		return errors.New("strict Sandbox Pod ownership is immutable")
	}
	if utils.MatchesGroupKind(metav1.GetControllerOf(sandbox), extensionsv1beta1.GroupVersion.Group, extensionsv1beta1.SandboxWarmPoolKind) && request.Operation == admissionv1.Update {
		if !reflect.DeepEqual(before.Spec, after.Spec) {
			return errors.New("unused strict pool execution cannot change its Pod specification")
		}
		if !sameAdmissionMetadata(&before, &after) && request.UserInfo.Username != h.ControllerUsername {
			return errors.New("unused strict pool metadata is controller-owned")
		}
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) sandboxForPod(ctx context.Context, pod *corev1.Pod) (*sandboxv1beta1.Sandbox, error) {
	owner := metav1.GetControllerOf(pod)
	if !utils.MatchesGroupKind(owner, sandboxv1beta1.GroupVersion.Group, "Sandbox") {
		return nil, nil
	}
	var sandbox sandboxv1beta1.Sandbox
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if sandbox.UID != owner.UID {
		return nil, errors.New("pod Sandbox owner UID does not match the current object")
	}
	return &sandbox, nil
}

func validateRuntimePodDeletion(sandbox *sandboxv1beta1.Sandbox) error {
	if status := sandbox.Status.RuntimeAdoption; status != nil && status.Reservation != nil && status.TerminalEvidenceDigest == "" {
		return errors.New("reserved Pod deletion requires confirmed runtime termination and root destruction")
	}
	return nil
}

func activationReadyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if string(pod.Status.Conditions[i].Type) == runtimeadoption.ActivationReadyCondition {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func (h *RuntimeAdoptionAdmissionHandler) validateNodeStatusRequest(ctx context.Context, request admission.Request) error {
	if request.UserInfo.Username != "system:serviceaccount:"+h.AgentNamespace+":gatekeeper-runtime-agent" {
		return errors.New("runtime node evidence requires the bound runtime agent identity")
	}
	extra := func(key string) (string, error) {
		values := request.UserInfo.Extra["authentication.kubernetes.io/"+key]
		if len(values) != 1 || values[0] == "" {
			return "", errors.New("runtime node evidence requires bound Pod and Node credential claims")
		}
		return values[0], nil
	}
	nodeName, err := extra("node-name")
	if err != nil {
		return err
	}
	nodeUID, err := extra("node-uid")
	if err != nil {
		return err
	}
	podName, err := extra("pod-name")
	if err != nil {
		return err
	}
	podUID, err := extra("pod-uid")
	if err != nil {
		return err
	}
	var node corev1.Node
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}
	var pod corev1.Pod
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: h.AgentNamespace, Name: podName}, &pod); err != nil {
		return err
	}
	if string(node.UID) != nodeUID || node.DeletionTimestamp != nil || string(pod.UID) != podUID || pod.DeletionTimestamp != nil ||
		pod.Spec.NodeName != nodeName || pod.Spec.ServiceAccountName != "gatekeeper-runtime-agent" {
		return errors.New("runtime evidence writer is not the live agent Pod on this Node UID")
	}
	data := request.Object.Raw
	if request.Operation == admissionv1.Delete {
		data = request.OldObject.Raw
	}
	var object unstructured.Unstructured
	if err := json.Unmarshal(data, &object.Object); err != nil {
		return err
	}
	var evidence runtimeadoption.NodeEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return err
	}
	if object.GetName() != nodeName || evidence.Spec.NodeRef.Name != nodeName || request.Name != "" && request.Name != nodeName ||
		request.Operation != admissionv1.Delete && evidence.Spec.NodeRef.UID != nodeUID {
		return errors.New("runtime node evidence names another bound Node")
	}
	if !slices.Contains([]admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete}, request.Operation) {
		return errors.New("unsupported runtime node evidence operation")
	}
	return nil
}
