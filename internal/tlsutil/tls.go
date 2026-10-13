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

package tlsutil

import (
	"crypto/tls"
	"fmt"
	"strings"
)

// ParseTLSVersion maps a version name to the corresponding crypto/tls
// constant. The accepted names match the Kubernetes API server's
// --tls-min-version flag.
func ParseTLSVersion(v string) (uint16, error) {
	switch v {
	case "VersionTLS10":
		return tls.VersionTLS10, nil
	case "VersionTLS11":
		return tls.VersionTLS11, nil
	case "VersionTLS12":
		return tls.VersionTLS12, nil
	case "VersionTLS13":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unknown TLS version %q; must be one of: VersionTLS10, VersionTLS11, VersionTLS12, VersionTLS13", v)
	}
}

// tls13CipherSuites lists the standard TLS 1.3 cipher suite names. Go
// manages these automatically and does not expose them via
// tls.CipherSuites(), but cluster TLS profiles may
// include them alongside TLS 1.2 suites. Recognizing them here lets a
// profile be injected without modification.
var tls13CipherSuites = map[string]bool{
	"TLS_AES_128_GCM_SHA256":       true,
	"TLS_AES_256_GCM_SHA384":       true,
	"TLS_CHACHA20_POLY1305_SHA256": true,
}

// IsTLS13CipherSuite reports whether name is a standard TLS 1.3 cipher
// suite that Go manages automatically and cannot be configured via
// tls.Config.CipherSuites.
func IsTLS13CipherSuite(name string) bool {
	return tls13CipherSuites[strings.TrimSpace(name)]
}

// ParseCipherSuites converts Go cipher-suite names to their numeric IDs.
// Unknown names cause an error. Names are looked up in
// tls.CipherSuites but not in tls.InsecureCipherSuites. Standard TLS
// 1.3 cipher suite names are recognized and silently skipped because Go
// manages them automatically.
func ParseCipherSuites(names []string) ([]uint16, error) {
	if len(names) == 0 {
		return nil, nil
	}

	supported := make(map[string]uint16)
	for _, cs := range tls.CipherSuites() {
		supported[cs.Name] = cs.ID
	}

	var ids []uint16
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if tls13CipherSuites[name] {
			continue
		}
		id, ok := supported[name]
		if !ok {
			return nil, fmt.Errorf("unknown cipher suite %q", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
