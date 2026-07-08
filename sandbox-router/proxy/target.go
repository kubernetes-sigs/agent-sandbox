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

package proxy

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// UpstreamURL builds the URL that requests should be forwarded to.
// scheme defaults to "http" when empty. clusterDomain is appended to the
// constructed FQDN when t.PodIP is unset.
//
// The path and rawQuery components are preserved verbatim from the inbound
// request so the upstream sandbox sees exactly what the client sent.
//
// This function does NOT consult any cache — it picks between the
// caller-supplied PodIP and the DNS form. Cache-aware resolution lives
// in (t Target) Resolve(...) in resolve.go.
func (t Target) UpstreamURL(scheme, clusterDomain, path, rawQuery string) *url.URL {
	if scheme == "" {
		scheme = "http"
	}
	host := t.PodIP
	if host == "" {
		host = fmt.Sprintf("%s.%s.svc.%s", t.ID, t.Namespace, clusterDomain)
	}
	return &url.URL{
		Scheme: scheme,
		// net.JoinHostPort brackets IPv6 literals — needed for sandbox
		// Pods on dual-stack / IPv6-only clusters where t.PodIP comes
		// from Pod.Status.PodIP as a bare IPv6 string.
		Host:     net.JoinHostPort(host, strconv.Itoa(t.Port)),
		Path:     path,
		RawQuery: rawQuery,
	}
}
