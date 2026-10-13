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
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsNamespaceTerminatingError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "standard generic error",
			err:      errors.New("generic error"),
			expected: false,
		},
		{
			name:     "status error with NamespaceTerminatingCause",
			err:      newNamespaceTerminatingError("test-ns1"),
			expected: true,
		},
		{
			name:     "wrapped NamespaceTerminatingCause error",
			err:      fmt.Errorf("reconcile failed: %w", newNamespaceTerminatingError("test-ns2")),
			expected: true,
		},
		{
			name: "status error with different cause",
			err: &k8serrors.StatusError{
				ErrStatus: metav1.Status{
					Status: metav1.StatusFailure,
					Code:   403,
					Reason: metav1.StatusReasonForbidden,
					Details: &metav1.StatusDetails{
						Causes: []metav1.StatusCause{
							{Type: metav1.CauseTypeFieldValueNotFound},
						},
					},
				},
			},
			expected: false,
		},
		{
			name:     "status error without causes",
			err:      k8serrors.NewNotFound(corev1.Resource("pods"), "test-pod"),
			expected: false,
		},
		{
			name: "joined error with NamespaceTerminating after another status error",
			err: errors.Join(
				k8serrors.NewNotFound(corev1.Resource("pods"), "test-pod"),
				newNamespaceTerminatingError("test-ns"),
			),
			expected: true,
		},
		{
			name: "joined error with wrapped NamespaceTerminating after generic error",
			err: errors.Join(
				errors.New("something went wrong"),
				fmt.Errorf("wrapped: %w", newNamespaceTerminatingError("test-ns")),
			),
			expected: true,
		},
		{
			name: "joined error without NamespaceTerminating",
			err: errors.Join(
				errors.New("err 1"),
				k8serrors.NewNotFound(corev1.Resource("pods"), "test-pod"),
			),
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNamespaceTerminatingError(tc.err)
			want := tc.expected
			if got != want {
				t.Errorf("%s: got %v, want %v", tc.name, got, want)
			}
		})
	}
}

func newNamespaceTerminatingError(ns string) error {
	return &k8serrors.StatusError{
		ErrStatus: metav1.Status{
			Status: metav1.StatusFailure,
			Code:   403,
			Reason: metav1.StatusReasonForbidden,
			Details: &metav1.StatusDetails{
				Causes: []metav1.StatusCause{
					{
						Type:    corev1.NamespaceTerminatingCause,
						Message: fmt.Sprintf("unable to create new content in namespace %s because it is being terminated", ns),
						Field:   "metadata.name",
					},
				},
			},
		},
	}
}
