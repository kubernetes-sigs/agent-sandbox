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

package sandbox

import (
	"flag"
	"os"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	// Skip goleak checks during live cluster integration tests to prevent
	// client-go / Kubernetes connection pools from flagging goroutine leaks.
	if isIntegrationTest() {
		os.Exit(m.Run())
	}
	goleak.VerifyTestMain(m)
}

func isIntegrationTest() bool {
	if os.Getenv("INTEGRATION_TEST") != "" {
		return true
	}
	// Use flag.Lookup to safely check flags that are only defined when build tag is 'integration'.
	if f := flag.Lookup("gateway-name"); f != nil && f.Value.String() != "" {
		return true
	}
	if f := flag.Lookup("api-url"); f != nil && f.Value.String() != "" {
		return true
	}
	return false
}
