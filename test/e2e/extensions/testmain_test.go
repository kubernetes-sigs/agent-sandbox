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

package extensions

import (
	"context"
	"fmt"
	"os"
	"testing"

	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

// TestMain scans the controller logs for RBAC denials after the whole suite
// has run. TestMain is the only hook guaranteed to run after parallel tests
// finish, and the scan rides on the suite's coverage: any code path that
// lost a needed RBAC verb — including paths that degrade silently, like
// event emission or best-effort patches — leaves an
// "is forbidden: User" line the suite itself would not otherwise notice.
func TestMain(m *testing.M) {
	code := m.Run()

	denials, err := framework.ScanControllerRBACDenials(context.Background())
	switch {
	case err != nil:
		// Don't mask test results over a log-fetch problem, but say so.
		fmt.Fprintf(os.Stderr, "WARNING: could not scan controller logs for RBAC denials: %v\n", err)
	case len(denials) > 0:
		fmt.Fprintf(os.Stderr, "FAIL: controller logs contain %d RBAC denial(s):\n", len(denials))
		for _, d := range denials {
			fmt.Fprintf(os.Stderr, "  %s\n", d)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
