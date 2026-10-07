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

import * as net from "node:net";
import type * as k8s from "@kubernetes/client-node";
import {
  SandboxConnectionError,
  type SandboxError,
  SandboxMetadataError,
  SandboxNoServiceError,
} from "./exceptions.js";
import { PodTunnel } from "./tunnel.js";
import type { Logger, SandboxdConnectivity } from "./types.js";

/**
 * The addresses one connection generation talks to sandboxd on, plus whatever
 * it owns to keep them reachable. Created fresh for every generation and
 * closed when that generation is torn down.
 * @internal — not part of the public API.
 */
export interface SandboxdTransport {
  readonly restBaseUrl: string;
  readonly grpcBaseUrl: string;
  /**
   * A failure observed on this transport that retrying the health check for
   * the rest of the connect budget cannot fix, or undefined.
   */
  terminalError(): SandboxError | undefined;
  close(): Promise<void>;
}

/**
 * Decides how a Sandbox handle reaches sandboxd. Mirrors the Go client's
 * ConnectionStrategy, but as a factory: the TS client builds a whole new
 * transport per connection generation rather than re-dialing one in place.
 * @internal — not part of the public API.
 */
export interface SandboxdConnectionStrategy {
  readonly connectivity: SandboxdConnectivity;
  open(): Promise<SandboxdTransport>;
}

/** @internal */
export interface ConnectionTarget {
  kubeConfig: k8s.KubeConfig;
  namespace: string;
  podName: string;
  podIP: string;
  serviceFQDN: string;
  restPort: number;
  grpcPort: number;
  /** Budget for each port-forward WS handshake. */
  handshakeTimeoutMs: number;
  logger: Logger;
}

// `ws` reports a rejected HTTP upgrade (the apiserver denying auth, or the
// Pod not existing) as an Error whose message is exactly
// `Unexpected server response: <status>` — see PodTunnel.lastHandshakeError.
// 401/403 (auth) and 404 (Pod gone) cannot be fixed by retrying; 5xx from the
// apiserver itself is left to the ordinary retry path since it may recover.
const TERMINAL_HANDSHAKE_STATUS_RE =
  /Unexpected server response: (401|403|404)\b/;

function isTerminalPortForwardFailure(err: unknown): boolean {
  if (!(err instanceof Error)) return false;
  return TERMINAL_HANDSHAKE_STATUS_RE.test(err.message);
}

/**
 * Reaches sandboxd through a WebSocket port-forward brokered by the
 * apiserver. Works from anywhere a kubeconfig does.
 * @internal
 */
export class PortForwardStrategy implements SandboxdConnectionStrategy {
  readonly connectivity = "port-forward";

  constructor(private readonly target: ConnectionTarget) {}

  async open(): Promise<SandboxdTransport> {
    const tunnel = new PodTunnel({
      kubeConfig: this.target.kubeConfig,
      namespace: this.target.namespace,
      podName: this.target.podName,
      restTargetPort: this.target.restPort,
      grpcTargetPort: this.target.grpcPort,
      handshakeTimeoutMs: this.target.handshakeTimeoutMs,
      logger: this.target.logger,
    });
    const endpoints = await tunnel.start();
    return {
      restBaseUrl: endpoints.restBaseUrl,
      grpcBaseUrl: endpoints.grpcBaseUrl,
      terminalError: () => {
        // A connection-level failure could just mean the port-forward
        // hasn't finished establishing yet — but if the apiserver has
        // already rejected the WS upgrade outright (auth denied, Pod not
        // found), retrying for the rest of the connect budget cannot help.
        const err = tunnel.lastHandshakeError;
        if (!isTerminalPortForwardFailure(err)) return undefined;
        return new SandboxConnectionError(
          "sandboxd port-forward was rejected by the apiserver",
          "port_forward",
          { cause: err },
        );
      },
      close: () => tunnel.close(),
    };
  }
}

/**
 * Dials sandboxd on the pod network, taking the apiserver off the data path.
 * Exactly one address is used, with no fallback between them: service mode
 * dials the headless Service by name (status.serviceFQDN), otherwise the pod
 * IP (status.podIPs). The caller must run inside the cluster.
 * @internal
 */
export class InClusterStrategy implements SandboxdConnectionStrategy {
  readonly connectivity: SandboxdConnectivity;

  constructor(
    private readonly target: ConnectionTarget,
    useServiceDNS: boolean,
  ) {
    this.connectivity = useServiceDNS
      ? "in-cluster-service"
      : "in-cluster-pod-ip";
  }

  async open(): Promise<SandboxdTransport> {
    const host = resolveInClusterHost(
      this.connectivity,
      this.target.podIP,
      this.target.serviceFQDN,
    );
    const hostPart = formatHost(host);
    return {
      restBaseUrl: `http://${hostPart}:${this.target.restPort}`,
      grpcBaseUrl: `http://${hostPart}:${this.target.grpcPort}`,
      // Nothing to classify: an unreachable pod address looks the same as a
      // sandboxd that is not listening yet, so the connect budget decides.
      terminalError: () => undefined,
      // Owns no listeners or sockets; the REST/gRPC clients built on these
      // URLs are torn down with their generation.
      close: async () => {},
    };
  }
}

/**
 * Returns the address in-cluster connectivity dials, or throws when the
 * Sandbox does not have it. Called both when a handle is created (to fail
 * early, like the Go client's Open) and on every connect.
 * @internal
 */
export function resolveInClusterHost(
  connectivity: SandboxdConnectivity,
  podIP: string,
  serviceFQDN: string,
): string {
  if (connectivity === "in-cluster-service") {
    if (!serviceFQDN) {
      throw new SandboxNoServiceError(
        'Sandbox has no headless Service, so it cannot be addressed by DNS; set spec.service: true on the template, or use "in-cluster-pod-ip" connectivity to dial the pod IP.',
      );
    }
    return serviceFQDN;
  }
  if (!podIP) {
    throw new SandboxMetadataError(
      "Sandbox pod IP is not resolved; cannot connect directly.",
    );
  }
  return podIP;
}

/** @internal */
export function createConnectionStrategy(
  connectivity: SandboxdConnectivity,
  target: ConnectionTarget,
): SandboxdConnectionStrategy {
  switch (connectivity) {
    case "in-cluster-pod-ip":
      return new InClusterStrategy(target, false);
    case "in-cluster-service":
      return new InClusterStrategy(target, true);
    default:
      return new PortForwardStrategy(target);
  }
}

/**
 * Picks the pod IP to dial from status.podIPs: the first valid IPv4 address,
 * otherwise the first valid address of any family. Mirrors the Go client's
 * selectPodIP so both SDKs pick the same address on dual-stack clusters.
 * @internal
 */
export function selectPodIP(ips: readonly unknown[]): string {
  let firstValid = "";
  for (const raw of ips) {
    if (typeof raw !== "string") continue;
    const ip = raw.trim();
    const family = net.isIP(ip);
    if (family === 4) return ip;
    if (family === 6 && firstValid === "") firstValid = ip;
  }
  return firstValid;
}

/** Brackets IPv6 literals for use in a URL authority. @internal */
export function formatHost(host: string): string {
  return net.isIPv6(host) ? `[${host}]` : host;
}
