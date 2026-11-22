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

package controllers

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// joinedError is implemented by errors.Join.
type joinedError interface {
	Unwrap() []error
}

// anyJoined reports whether match holds for err or any error in an
// errors.Join tree. The k8serrors helpers only inspect the first status in a
// joined error.
func anyJoined(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}

	if joined, ok := err.(joinedError); ok {
		return slices.ContainsFunc(joined.Unwrap(), func(e error) bool { return anyJoined(e, match) })
	}

	return match(err)
}

// isNamespaceTerminatingError reports whether err or any error in its chain
// indicates that the namespace is terminating (e.g. 403 Forbidden with
// NamespaceTerminating cause, or wrapping ErrNamespaceTerminating).
func isNamespaceTerminatingError(err error) bool {
	return anyJoined(err, func(e error) bool {
		return k8serrors.HasStatusCause(e, corev1.NamespaceTerminatingCause)
	})
}

// isForbiddenError reports whether err or any joined error is Forbidden.
func isForbiddenError(err error) bool {
	return anyJoined(err, k8serrors.IsForbidden)
}
