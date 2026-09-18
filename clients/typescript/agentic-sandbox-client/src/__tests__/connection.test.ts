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

import { describe, expect, it } from "vitest";
import {
  createConnectionStrategy,
  formatHost,
  InClusterStrategy,
  PortForwardStrategy,
  resolveInClusterHost,
  selectPodIP,
} from "../connection.js";
import { SandboxMetadataError, SandboxNoServiceError } from "../exceptions.js";
import { noopLogger } from "../logger.js";

describe("selectPodIP", () => {
  it.each([
    ["empty list", [], ""],
    ["single IPv4", ["10.0.0.1"], "10.0.0.1"],
    ["single IPv6", ["fd00::1"], "fd00::1"],
    ["prefers IPv4 over an earlier IPv6", ["fd00::1", "10.0.0.1"], "10.0.0.1"],
    ["first IPv4 wins", ["10.0.0.1", "10.0.0.2"], "10.0.0.1"],
    ["first valid IPv6 when no IPv4", ["fd00::1", "fd00::2"], "fd00::1"],
    ["skips invalid entries", ["not-an-ip", "", "10.0.0.3"], "10.0.0.3"],
    ["all invalid", ["bogus", "999.1.1.1"], ""],
    ["trims whitespace", ["  10.0.0.4  "], "10.0.0.4"],
    ["ignores non-string entries", [42, null, "10.0.0.5"], "10.0.0.5"],
  ] as const)("%s", (_name, ips, want) => {
    expect(selectPodIP(ips)).toBe(want);
  });
});

describe("formatHost", () => {
  it.each([
    ["10.0.0.1", "10.0.0.1"],
    ["fd00::1", "[fd00::1]"],
    ["sb.default.svc.cluster.local", "sb.default.svc.cluster.local"],
  ])("%s -> %s", (host, want) => {
    expect(formatHost(host)).toBe(want);
  });
});

describe("resolveInClusterHost", () => {
  it("service mode dials the Service FQDN even when a pod IP is known", () => {
    expect(
      resolveInClusterHost("in-cluster-service", "10.0.0.1", "sb.ns.svc"),
    ).toBe("sb.ns.svc");
  });

  it("service mode never falls back to the pod IP", () => {
    expect(() =>
      resolveInClusterHost("in-cluster-service", "10.0.0.1", ""),
    ).toThrow(SandboxNoServiceError);
  });

  it("pod-IP mode dials the pod IP even when a Service exists", () => {
    expect(
      resolveInClusterHost("in-cluster-pod-ip", "10.0.0.1", "sb.ns.svc"),
    ).toBe("10.0.0.1");
  });

  it("pod-IP mode reports a missing IP distinctly from a missing Service", () => {
    let err: unknown;
    try {
      resolveInClusterHost("in-cluster-pod-ip", "", "sb.ns.svc");
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(SandboxMetadataError);
    expect(err).not.toBeInstanceOf(SandboxNoServiceError);
  });
});

describe("createConnectionStrategy", () => {
  const target = {
    kubeConfig: {} as never,
    namespace: "default",
    podName: "pod",
    podIP: "fd00::9",
    serviceFQDN: "sb.default.svc.cluster.local",
    restPort: 8080,
    grpcPort: 9090,
    handshakeTimeoutMs: 1000,
    logger: noopLogger,
  };

  it("selects port-forward by default", () => {
    const strategy = createConnectionStrategy("port-forward", target);
    expect(strategy).toBeInstanceOf(PortForwardStrategy);
    expect(strategy.connectivity).toBe("port-forward");
  });

  it("builds bracketed pod-IP URLs for IPv6 in pod-IP mode", async () => {
    const strategy = createConnectionStrategy("in-cluster-pod-ip", target);
    expect(strategy).toBeInstanceOf(InClusterStrategy);
    const transport = await strategy.open();
    expect(transport.restBaseUrl).toBe("http://[fd00::9]:8080");
    expect(transport.grpcBaseUrl).toBe("http://[fd00::9]:9090");
    expect(transport.terminalError()).toBeUndefined();
    await transport.close();
  });

  it("builds Service URLs in service mode", async () => {
    const strategy = createConnectionStrategy("in-cluster-service", target);
    expect(strategy.connectivity).toBe("in-cluster-service");
    const transport = await strategy.open();
    expect(transport.restBaseUrl).toBe(
      "http://sb.default.svc.cluster.local:8080",
    );
    expect(transport.grpcBaseUrl).toBe(
      "http://sb.default.svc.cluster.local:9090",
    );
  });
});
