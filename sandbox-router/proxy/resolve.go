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
	"net"
	"net/url"
	"strconv"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
)

// Lookup is the slice of the Pod-IP cache the proxy depends on. Defined
// as an interface so the handler can be tested with a fake and so the
// proxy package doesn't pull the informer wiring into its dependency
// graph just to make a map read.
type Lookup interface {
	Get(uid types.UID) (cache.Entry, bool)
	// Invalidate evicts an entry; called by the proxy's ErrorHandler on
	// dial-class failures so the next request doesn't retry the stale IP.
	Invalidate(uid types.UID) bool
}

// Source tags how the upstream host was picked. Returned alongside the
// resolved URL so the handler can log/metric the resolution mode.
type Source string

const (
	// SourcePodIP — caller passed X-Sandbox-Pod-IP and we used it
	// directly. Skips both cache and DNS.
	SourcePodIP Source = "pod-ip"
	// SourceCache — UID was present and matched a cache entry; we dialed
	// the live Pod IP. The KEP-NNNN fast/secure path.
	SourceCache Source = "cache"
	// SourceDNS — UID was absent or cache-missed; fell back to the
	// in-cluster DNS form <id>.<ns>.svc.<cluster-domain>:<port>.
	SourceDNS Source = "dns"
)

// Resolve picks the upstream host+port for a Target and returns the full
// URL ready to hand to httputil.ReverseProxy. Resolution priority is
// stable and intentional:
//
//  1. t.PodIP (set from X-Sandbox-Pod-IP) — explicit caller override,
//     used by SDKs that already know the Pod IP from creating the Sandbox.
//  2. cache lookup by t.UID — KEP-NNNN's secure fast path. Only attempted
//     when both cache is non-nil AND t.UID is present.
//  3. DNS form — always works without informer cache or UID, matches
//     the Python router's behavior.
//
// scheme defaults to "http" when empty. The returned Source records
// which branch fired so the caller can attribute logs and metrics.
func (t Target) Resolve(scheme, clusterDomain, path, rawQuery string, lookup Lookup) (*url.URL, Source) {
	if scheme == "" {
		scheme = "http"
	}

	var host string
	src := SourceDNS
	switch {
	case t.PodIP != "":
		host = t.PodIP
		src = SourcePodIP
	case lookup != nil && t.UID != "":
		if e, ok := lookup.Get(types.UID(t.UID)); ok {
			host = e.PodIP
			src = SourceCache
		}
	}
	if host == "" {
		// DNS fallback. This branch fires when there was no PodIP override
		// and either the cache wasn't configured, the UID wasn't supplied,
		// or the cache missed.
		host = t.ID + "." + t.Namespace + ".svc." + clusterDomain
	}

	return &url.URL{
		Scheme: scheme,
		// net.JoinHostPort brackets IPv6 literals per RFC 3986. Pod IPs
		// on dual-stack or IPv6-only clusters surface as bare IPv6
		// strings in Pod.Status.PodIP, and an unbracketed "::1:8080" is
		// ambiguous with the address itself; net/http would fail to
		// parse the resulting URL.
		Host:     net.JoinHostPort(host, strconv.Itoa(t.Port)),
		Path:     path,
		RawQuery: rawQuery,
	}, src
}
