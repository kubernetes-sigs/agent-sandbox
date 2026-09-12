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

package main

import (
	"crypto/tls"
	"strings"

	inttls "sigs.k8s.io/agent-sandbox/internal/tlsutil"
)

func buildMetricsTLSOpts(minVersion, cipherSuites string) ([]func(*tls.Config), error) {
	var opts []func(*tls.Config)

	var minVer uint16
	if minVersion != "" {
		var err error
		minVer, err = inttls.ParseTLSVersion(minVersion)
		if err != nil {
			return nil, err
		}
		opts = append(opts, func(c *tls.Config) {
			c.MinVersion = minVer
		})
	}

	if cipherSuites != "" {
		names := strings.Split(cipherSuites, ",")
		ids, err := inttls.ParseCipherSuites(names)
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 && minVer < tls.VersionTLS13 {
			opts = append(opts, func(c *tls.Config) {
				c.CipherSuites = ids
			})
		}
	}

	return opts, nil
}
