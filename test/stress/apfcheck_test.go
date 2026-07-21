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

import (
	"net/http"
	"testing"
)

type stubRoundTripper struct {
	responses []*http.Response
	i         int
}

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	resp := s.responses[s.i%len(s.responses)]
	s.i++
	return resp, nil
}

func respWithPF(fsUID, plUID string) *http.Response {
	h := http.Header{}
	if fsUID != "" {
		h.Set(pfFlowSchemaHeader, fsUID)
		h.Set(pfPriorityLevelHeader, plUID)
	}
	return &http.Response{StatusCode: 200, Header: h}
}

// TestPFHeaderCaptureRecordsClassification pins the two properties the
// preflight relies on: header names match what kube-apiserver emits
// (case-insensitive Get), and responses without APF headers (e.g. the
// discovery calls the dynamic client makes before the dry-run POST) never
// clobber a captured classification.
func TestPFHeaderCaptureRecordsClassification(t *testing.T) {
	capture := &pfHeaderCapture{base: &stubRoundTripper{responses: []*http.Response{
		respWithPF("", ""),                 // discovery: no APF headers
		respWithPF("fs-uid-1", "pl-uid-1"), // the dry-run create
		respWithPF("", ""),                 // trailing header-less response
	}}}

	req := &http.Request{}
	for range 3 {
		if _, err := capture.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
	}
	fs, pl := capture.uids()
	if fs != "fs-uid-1" || pl != "pl-uid-1" {
		t.Errorf("captured (%q, %q), want (fs-uid-1, pl-uid-1)", fs, pl)
	}
}
