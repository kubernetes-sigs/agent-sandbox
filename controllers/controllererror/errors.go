// Copyright 2025 The Kubernetes Authors.
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

package controllererror

import (
	"errors"
	"fmt"
)

// TerminalError marks an error as non-retryable: the reconciler should surface
// it in status/events but skip requeue loops because retries cannot fix it.
type TerminalError struct {
	msg string
}

func (e TerminalError) Error() string { return e.msg }

// NewTerminalError wraps a message so FilterTerminalErrors can drop it from
// the returned error while still allowing status/conditions to record it.
func NewTerminalError(format string, args ...interface{}) error {
	return TerminalError{msg: fmt.Sprintf(format, args...)}
}

// FilterTerminalErrors removes TerminalError instances from a (possibly joined)
// error tree; remaining errors are returned for requeue.
func FilterTerminalErrors(err error) error {
	if err == nil {
		return nil
	}
	var normalErrs error
	for _, e := range unwrapAll(err) {
		if isTerminalError(e) {
			continue
		}
		normalErrs = errors.Join(normalErrs, e)
	}
	return normalErrs
}

func isTerminalError(err error) bool {
	var imm TerminalError
	return errors.As(err, &imm)
}

func unwrapAll(err error) []error {
	if err == nil {
		return nil
	}

	// Handle errors.Join (implements Unwrap() []error)
	type unwrapper interface {
		Unwrap() []error
	}
	if uw, ok := err.(unwrapper); ok {
		var res []error
		for _, e := range uw.Unwrap() {
			res = append(res, unwrapAll(e)...)
		}
		return res
	}

	// Fallback to single unwrap chain
	if u := errors.Unwrap(err); u != nil {
		return append(unwrapAll(u), err)
	}
	return []error{err}
}
