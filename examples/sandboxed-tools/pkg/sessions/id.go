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

package sessions

import "fmt"

// ValidateSessionName validates that the session name is valid.
// We use session names in kubernetes and in filesystem paths,
// so we limit the characters allowed (lowercase alphanumeric) and the length (max 40 characters).
func ValidateSessionName(sessionName string) error {
	if !isLowerAlphaNumeric(sessionName) {
		return fmt.Errorf("session name must be lowercase alphanumeric")
	}

	// Limit length; erring on the side of caution.
	if len(sessionName) > 40 {
		return fmt.Errorf("session name must be no more than 40 characters long")
	}

	return nil
}

// isLowerAlphaNumeric returns true if the string is purely lower-case alphanumeric.
func isLowerAlphaNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
