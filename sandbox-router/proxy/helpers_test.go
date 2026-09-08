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

// Test helpers shared between unit and integration tests in this
// package. No build tag → visible to both `go test ./...` and
// `go test -tags=integration ./...`.

package proxy

import (
	"net"
	"strconv"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
)

// pickFreePort reserves an ephemeral port and immediately closes the
// listener, returning the port number. The port is free at the moment
// of return — there's a tiny race where another process on the host
// could grab it before the caller dials, but in the controlled test
// environments we run in (single host, no other process binding
// arbitrary ephemeral ports during the test) it's deterministic.
//
// Used for two cases:
//   - startDelayedBackend (retry_integration_test.go): pre-reserve a
//     port so the caller can build the request URL before the backend
//     actually starts listening.
//   - "guaranteed dead destination" in tests that want to force a
//     dial failure without depending on whether anything happens to
//     be listening on a hardcoded port like 1.
func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// pickFreePortStr is pickFreePort returning the port as a string,
// convenient for header values.
func pickFreePortStr(t *testing.T) string {
	t.Helper()
	return strconv.Itoa(pickFreePort(t))
}

// stubLookup is a minimal Lookup for tests of cache wiring, both plain
// unit tests and `-tags=integration` ones. No build tag on this file, so
// it's visible from either. GetByName scans entries so tests only have to
// populate one map. A mutex guards the maps/slices because the handler
// mutates them from the httptest server goroutine while assertions read
// from the test goroutine.
type stubLookup struct {
	mu                sync.Mutex
	entries           map[types.UID]cache.Entry
	invalidated       []types.UID
	invalidatedByName []string
}

func (s *stubLookup) Resolve(namespace, name string, requestedUID types.UID) (cache.Entry, cache.ResolutionSource, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, e := range s.entries {
		if e.Namespace == namespace && e.SandboxName == name {
			e.SandboxUID = uid
			if requestedUID != "" && requestedUID == uid {
				return e, cache.ResolutionByUID, true
			}
			return e, cache.ResolutionByName, true
		}
	}
	if requestedUID != "" {
		if e, ok := s.entries[requestedUID]; ok {
			if e.Namespace == "" && e.SandboxName == "" {
				e.Namespace = namespace
				e.SandboxName = name
			}
			if e.Namespace != namespace || e.SandboxName != name {
				return cache.Entry{}, 0, false
			}
			e.SandboxUID = requestedUID
			return e, cache.ResolutionByUID, true
		}
	}
	return cache.Entry{}, 0, false
}

func (s *stubLookup) Get(uid types.UID) (cache.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uid]
	return e, ok
}

func (s *stubLookup) GetByName(namespace, name string) (cache.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Namespace == namespace && e.SandboxName == name {
			return e, true
		}
	}
	return cache.Entry{}, false
}

func (s *stubLookup) Invalidate(uid types.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[uid]
	delete(s.entries, uid)
	s.invalidated = append(s.invalidated, uid)
	return ok
}

func (s *stubLookup) InvalidateByName(namespace, name, podIP string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidatedByName = append(s.invalidatedByName, namespace+"/"+name+"="+podIP)
	for uid, e := range s.entries {
		if e.Namespace == namespace && e.SandboxName == name && e.PodIP == podIP {
			delete(s.entries, uid)
			return true
		}
	}
	return false
}
