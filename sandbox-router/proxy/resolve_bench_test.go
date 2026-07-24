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

// Benchmarks for the three upstream-resolution paths, in the order the
// proxy tries them after the X-Sandbox-Pod-IP override: UID cache hit,
// namespace/name cache hit, and DNS fallback.
//
// Target.Resolve does not perform DNS itself — on the DNS path it only
// constructs the <id>.<ns>.svc.<cluster-domain> URL and the actual
// lookup happens later, inside the transport's dialer. The resolver is
// therefore not injectable into Resolve; BenchmarkResolveDNSFallback
// composes Resolve with a direct net.Resolver lookup against a loopback
// UDP DNS stub to measure the full resolution work a DNS-path request
// pays before a connection can be opened. That is the BEST-CASE DNS
// cost: the stub answers immediately on 127.0.0.1, while real cluster
// DNS adds at least one network round trip per query — and for
// Sandboxes with spec.service: false the name does not exist at all,
// producing the NXDOMAIN → 502 failure mode described in #883.

package proxy

import (
	"context"
	"net"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
)

func BenchmarkResolveUIDCacheHit(b *testing.B) {
	lookup := &fakeLookup{
		entries: map[types.UID]cache.Entry{"u1": {PodIP: "10.0.0.42", SandboxName: "id", Namespace: "ns"}},
	}
	tgt := Target{ID: "id", UID: "u1", Namespace: "ns", Port: 8080}

	b.ReportAllocs()
	for b.Loop() {
		u, src := tgt.Resolve("http", "cluster.local", "/run", "", lookup)
		if src != SourceCache {
			b.Fatalf("source: got %q want %q", src, SourceCache)
		}
		_ = u
	}
}

func BenchmarkResolveNameCacheHit(b *testing.B) {
	// No UID header (the common SDK case): resolution falls through the
	// UID branch and hits the namespace/name index.
	lookup := &fakeLookup{
		byName: map[string]cache.Entry{"ns/id": {PodIP: "10.0.0.42", SandboxName: "id", Namespace: "ns"}},
	}
	tgt := Target{ID: "id", Namespace: "ns", Port: 8080}

	b.ReportAllocs()
	for b.Loop() {
		u, src := tgt.Resolve("http", "cluster.local", "/run", "", lookup)
		if src != SourceCacheName {
			b.Fatalf("source: got %q want %q", src, SourceCacheName)
		}
		_ = u
	}
}

func BenchmarkResolveDNSFallback(b *testing.B) {
	stubAddr := startDNSStub(b)
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", stubAddr)
		},
	}
	tgt := Target{ID: "id", Namespace: "ns", Port: 8080}
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		u, src := tgt.Resolve("http", "cluster.local", "/run", "", nil)
		if src != SourceDNS {
			b.Fatalf("source: got %q want %q", src, SourceDNS)
		}
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			b.Fatalf("split host: %v", err)
		}
		// Trailing dot: query the rooted name so the host's resolv.conf
		// search list can't perturb the measurement.
		addrs, err := resolver.LookupHost(ctx, host+".")
		if err != nil || len(addrs) == 0 {
			b.Fatalf("lookup %q: addrs=%v err=%v", host, addrs, err)
		}
	}
}

// startDNSStub runs a minimal UDP DNS server on 127.0.0.1:0 that answers
// every A/AAAA question immediately with a loopback address, and returns
// its address. It exists so BenchmarkResolveDNSFallback measures the
// resolver code path without depending on (or perturbing) real DNS.
func startDNSStub(b *testing.B) string {
	b.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen udp: %v", err)
	}
	b.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // listener closed
			}
			if resp := dnsStubAnswer(buf[:n]); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// dnsStubAnswer builds a NOERROR response to a single-question DNS query:
// the question echoed back plus one answer record of the queried type
// (A → 127.0.0.1, AAAA → ::1) pointing at the question name. Returns nil
// for packets it can't parse. Hand-rolled (12-byte header, label walk,
// compression pointer to offset 12) to avoid pulling a DNS library into
// the module just for a benchmark stub.
func dnsStubAnswer(q []byte) []byte {
	const hdr = 12
	if len(q) < hdr+5 { // header + at least a root label and QTYPE/QCLASS
		return nil
	}
	// Walk the QNAME labels to find the end of the question section.
	i := hdr
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	qend := i + 1 + 4 // zero byte + QTYPE + QCLASS
	if qend > len(q) {
		return nil
	}
	qtype := uint16(q[qend-4])<<8 | uint16(q[qend-3])

	resp := make([]byte, 0, qend+30)
	resp = append(resp,
		q[0], q[1], // ID echoed from the query
		0x81, 0x80, // QR=1, RD=1, RA=1, RCODE=NOERROR
		0, 1, // QDCOUNT
		0, 1, // ANCOUNT
		0, 0, // NSCOUNT
		0, 0, // ARCOUNT
	)
	resp = append(resp, q[hdr:qend]...) // question echoed back
	resp = append(resp,
		0xC0, hdr, // NAME: compression pointer to the question name
		q[qend-4], q[qend-3], // TYPE: echo QTYPE
		0, 1, // CLASS IN
		0, 0, 0, 30, // TTL
	)
	if qtype == 28 { // AAAA
		resp = append(resp, 0, 16)
		resp = append(resp, net.IPv6loopback...)
	} else { // treat everything else as A
		resp = append(resp, 0, 4, 127, 0, 0, 1)
	}
	return resp
}
