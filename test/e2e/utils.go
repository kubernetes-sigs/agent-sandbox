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

package e2e

import (
	"sync/atomic"
	"time"
)

// AtomicTimeDuration is a wrapper around time.Duration that allows for concurrent updates and retrievals.
type AtomicTimeDuration struct {
	v uint64
}

// Seconds returns the duration in seconds as a float64.
func (s *AtomicTimeDuration) Seconds() float64 {
	v := atomic.LoadUint64(&s.v)
	d := time.Duration(v)
	return d.Seconds()
}

// IsEmpty returns true if the duration is zero.
func (s *AtomicTimeDuration) IsEmpty() bool {
	return atomic.LoadUint64(&s.v) == 0
}

// Set sets the duration to the given value.
func (s *AtomicTimeDuration) Set(d time.Duration) {
	atomic.StoreUint64(&s.v, uint64(d))
}

// String returns the duration as a string.
func (s *AtomicTimeDuration) String() string {
	v := atomic.LoadUint64(&s.v)
	d := time.Duration(v)
	return d.String()
}
