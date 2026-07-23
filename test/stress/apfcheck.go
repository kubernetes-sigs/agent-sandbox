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

package main

// APF priority-level preflight.
//
// The claims phases' create-ack numbers assume the harness's claim POSTs
// ride the APF *exempt* priority level (admin kubeconfig -> system:masters
// -> the built-in "exempt" FlowSchema). A mis-provisioned kubeconfig
// silently shifts every create onto a queued priority level and poisons
// the run's create-ack numbers without any visible error.
//
// The apiserver reports the APF classification of EVERY request in the
// X-Kubernetes-PF-FlowSchema-UID / X-Kubernetes-PF-PriorityLevel-UID
// response headers, so the check is one server-side DRY-RUN SandboxClaim
// create through a header-capturing transport (classification happens in
// the filter chain, well before validation/storage — the dry-run never
// persists anything and the result is valid even if the create itself is
// rejected). UIDs are resolved to names via the flowcontrol API and the
// result lands in summary.json (Summary.APFVerification) and the run log.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	pfFlowSchemaHeader    = "X-Kubernetes-PF-FlowSchema-UID"
	pfPriorityLevelHeader = "X-Kubernetes-PF-PriorityLevel-UID"
	// exemptPriorityLevelName is the built-in APF priority level that
	// bypasses queuing entirely; the bench contract is that harness claim
	// POSTs classify here (admin kubeconfig -> system:masters).
	exemptPriorityLevelName = "exempt"
)

var (
	gvrFlowSchemas = schema.GroupVersionResource{
		Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"}
	gvrPriorityLevels = schema.GroupVersionResource{
		Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "prioritylevelconfigurations"}
)

// APFVerification is the recorded result of the preflight (summary.json).
type APFVerification struct {
	// FlowSchema / PriorityLevel are the resolved object NAMES; the UIDs are
	// kept alongside in case name resolution failed mid-run.
	FlowSchema       string `json:"flowSchema,omitempty"`
	FlowSchemaUID    string `json:"flowSchemaUID,omitempty"`
	PriorityLevel    string `json:"priorityLevel,omitempty"`
	PriorityLevelUID string `json:"priorityLevelUID,omitempty"`
	// Exempt reports whether the claim POST classified into the exempt
	// priority level — the bench-calibration contract.
	Exempt bool `json:"exempt"`
	// Error records a non-fatal preflight failure (the run proceeds and is
	// annotated as unverified rather than aborted).
	Error string `json:"error,omitempty"`
}

// pfHeaderCapture records the APF classification headers of the last
// response that carried them.
type pfHeaderCapture struct {
	base http.RoundTripper

	mu    sync.Mutex
	fsUID string
	plUID string
}

func (h *pfHeaderCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.base.RoundTrip(req)
	if resp != nil {
		if fs := resp.Header.Get(pfFlowSchemaHeader); fs != "" {
			h.mu.Lock()
			h.fsUID = fs
			h.plUID = resp.Header.Get(pfPriorityLevelHeader)
			h.mu.Unlock()
		}
	}
	return resp, err
}

func (h *pfHeaderCapture) uids() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fsUID, h.plUID
}

// verifyClaimPostPriorityLevel dry-run-creates one SandboxClaim in the given
// (existing) namespace through a header-capturing copy of the harness's rest
// config and resolves the APF classification. It never fails the run: any
// error is recorded in the returned struct.
func verifyClaimPostPriorityLevel(ctx context.Context, baseConfig *rest.Config, namespace string) *APFVerification {
	result := &APFVerification{}

	capture := &pfHeaderCapture{}
	cfg := rest.CopyConfig(baseConfig)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		capture.base = rt
		return capture
	})
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		result.Error = fmt.Sprintf("build preflight client: %v", err)
		return result
	}

	claim := buildClaimObject(
		types.NamespacedName{Namespace: namespace, Name: "apf-preflight"},
		"apf-preflight-pool")
	// Server-side dry run: full authn/authz/APF/admission traversal, nothing
	// persisted. The PF headers are set regardless of the create's outcome,
	// so a rejection (e.g. future CRD validation) does not void the check.
	_, createErr := client.Resource(gvrSandboxClaims).Namespace(namespace).
		Create(ctx, claim, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})

	fsUID, plUID := capture.uids()
	if fsUID == "" {
		if createErr != nil {
			result.Error = fmt.Sprintf("dry-run claim create returned no APF headers (err: %v)", createErr)
		} else {
			result.Error = "dry-run claim create returned no APF headers (APF disabled?)"
		}
		return result
	}
	result.FlowSchemaUID, result.PriorityLevelUID = fsUID, plUID

	result.FlowSchema = resolveUIDToName(ctx, client, gvrFlowSchemas, fsUID)
	result.PriorityLevel = resolveUIDToName(ctx, client, gvrPriorityLevels, plUID)
	result.Exempt = result.PriorityLevel == exemptPriorityLevelName
	return result
}

// resolveUIDToName lists the given cluster-scoped resource and returns the
// name of the object with the given UID ("" if not found / list failed).
func resolveUIDToName(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, uid string) string {
	list, err := client.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	for _, item := range list.Items {
		if string(item.GetUID()) == uid {
			return item.GetName()
		}
	}
	return ""
}

// logAPFVerification prints the preflight verdict in run-log-greppable form.
func logAPFVerification(v *APFVerification) {
	switch {
	case v.Error != "":
		log.Printf("APF preflight: UNVERIFIED (%s) — create-ack numbers may include priority-level queueing", v.Error)
	case v.Exempt:
		log.Printf("APF preflight: claim POSTs ride flowSchema=%q priorityLevel=%q (exempt: no APF queueing on the create path)",
			v.FlowSchema, v.PriorityLevel)
	default:
		log.Printf("APF preflight WARNING: claim POSTs classify into flowSchema=%q priorityLevel=%q (uid %s), NOT the exempt level — "+
			"create-ack measurements will include APF seat contention; check the kubeconfig user (expected system:masters)",
			v.FlowSchema, v.PriorityLevel, v.PriorityLevelUID)
	}
}
