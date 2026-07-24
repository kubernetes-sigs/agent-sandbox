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

package tools

import "context"

// fakeResponse is a canned result for one fakeSandbox.ExecCommand call.
type fakeResponse struct {
	result *ExecCommandResult
	err    error
}

// fakeSandbox is a test double for the Sandbox interface. It records every
// call it receives (in order) and returns responses queued up front, one
// per call. Tools that call ExecCommand more than once (e.g. WriteFileTool)
// can be exercised by queueing multiple responses.
type fakeSandbox struct {
	calls     []ExecCommandOptions
	responses []fakeResponse
}

func (f *fakeSandbox) ExecCommand(ctx context.Context, opts ExecCommandOptions) (*ExecCommandResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, opts)

	if i >= len(f.responses) {
		return &ExecCommandResult{}, nil
	}
	resp := f.responses[i]
	if resp.err != nil {
		return nil, resp.err
	}
	return resp.result, nil
}
