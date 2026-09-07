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

import (
	"context"
	"fmt"
)

// fakeResponse is a canned result for one fakeSandbox.ExecCommand call.
type fakeResponse struct {
	result *ExecCommandResult
	err    error

	// block, if true, makes ExecCommand ignore result/err and instead block
	// until the call's context is done, then return its Err(). This lets
	// tests deterministically exercise cancellation/timeout behavior without
	// real sleeps or polling.
	block bool
}

// fakeSandbox is a test double for the Sandbox interface. It records every
// call it receives (in order) and returns responses queued up front, one
// per call. Tools that call ExecCommand more than once (e.g. WriteFileTool)
// can be exercised by queueing multiple responses.
type fakeSandbox struct {
	calls     []ExecCommandOptions
	ctxs      []context.Context
	responses []fakeResponse
}

func (f *fakeSandbox) ExecCommand(ctx context.Context, opts ExecCommandOptions) (*ExecCommandResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, opts)
	f.ctxs = append(f.ctxs, ctx)

	if i >= len(f.responses) {
		return nil, fmt.Errorf("unexpected ExecCommand call %d", i+1)
	}
	resp := f.responses[i]
	if resp.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if resp.err != nil {
		return nil, resp.err
	}
	return resp.result, nil
}
