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

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
