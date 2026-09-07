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
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// gRPC (used by the sandboxd runtime tests) keeps an internal
	// CallbackSerializer goroutine that is not guaranteed to have exited by
	// the time goleak inspects at process exit, even after ClientConn.Close.
	// Ignore that known background goroutine; connections are still closed
	// by the connector and the tests' own cleanup.
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
	)
}
