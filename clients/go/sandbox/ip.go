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
	"net"
	"strings"
)

// selectPodIP scans the list of IP addresses, validates them, and returns
// the normalized/canonical IP address (preferring IPv4 over IPv6).
// In dual-stack environments, we explicitly prefer IPv4 over IPv6 for the
// X-Sandbox-Pod-IP header. This is a Go-specific optimization/precedence rule that
// ensures maximum compatibility with the downstream router's routing handler.
// If no IPv4 is found, it falls back to the first syntactically valid IP.
func selectPodIP(ips []string) string {
	var firstValid string
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		parsed := net.ParseIP(ip)
		if parsed != nil {
			if parsed.To4() != nil {
				return parsed.To4().String() // IPv4 has highest precedence; stop scanning.
			}
			if firstValid == "" {
				firstValid = parsed.String()
			}
		}
	}
	return firstValid
}
