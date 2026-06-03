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
)

func TestSelectPodIP(t *testing.T) {
	cases := []struct {
		name     string
		ips      []string
		expected string
	}{
		{
			name:     "no IPs",
			ips:      nil,
			expected: "",
		},
		{
			name:     "single valid IPv4",
			ips:      []string{"10.244.0.42"},
			expected: "10.244.0.42",
		},
		{
			name:     "single valid IPv6",
			ips:      []string{"2001:db8::1"},
			expected: "2001:db8::1",
		},
		{
			name:     "dual-stack prioritizing IPv4 first",
			ips:      []string{"10.244.0.42", "2001:db8::1"},
			expected: "10.244.0.42",
		},
		{
			name:     "dual-stack prioritizing IPv4 when IPv6 is first",
			ips:      []string{"2001:db8::1", "10.244.0.42"},
			expected: "10.244.0.42",
		},
		{
			name:     "multiple IPv6 selects first parseable",
			ips:      []string{"2001:db8::1", "2001:db8::2"},
			expected: "2001:db8::1",
		},
		{
			name:     "ignores invalid IP and falls back to valid IPv4",
			ips:      []string{"not-a-valid-ip", "10.244.0.42"},
			expected: "10.244.0.42",
		},
		{
			name:     "ignores invalid IP and falls back to valid IPv6",
			ips:      []string{"not-a-valid-ip", "2001:db8::1"},
			expected: "2001:db8::1",
		},
		{
			name:     "all invalid IPs leaves it empty",
			ips:      []string{"not-a-valid-ip", "bad-address"},
			expected: "",
		},
		{
			name:     "normalizes IP address format (IPv4 whitespace)",
			ips:      []string{"  192.168.1.1  "},
			expected: "192.168.1.1",
		},
		{
			name:     "normalizes IP address format (IPv6 compression)",
			ips:      []string{"2001:db8:0:0:0:0:2:1"},
			expected: "2001:db8::2:1",
		},
		{
			name:     "normalizes IP address format (IPv4-mapped IPv6)",
			ips:      []string{"::ffff:10.0.0.1"},
			expected: "10.0.0.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectPodIP(tc.ips)
			if got != tc.expected {
				t.Errorf("expected selectPodIP %q, got %q", tc.expected, got)
			}
		})
	}
}
