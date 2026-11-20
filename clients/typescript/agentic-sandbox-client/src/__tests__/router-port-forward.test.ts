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

import * as http from "node:http";
import * as net from "node:net";
import * as k8s from "@kubernetes/client-node";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WebSocket as WSType } from "ws";
import { WebSocketServer } from "ws";
import { SandboxClosedError, SandboxTimeoutError } from "../exceptions.js";
import { noopLogger } from "../logger.js";
import {
  normalizeSandboxdOptions,
  Sandbox,
  type SandboxInit,
} from "../sandbox.js";

const ROUTER_NAMESPACE = "agent-sandbox-system";
const ROUTER_POD = "router-pod-abc";
const REST_PORT = 18080;
const GRPC_PORT = 19090;

// ---------- fake router: a plain HTTP server that records the routing headers
// the SDK must send, and answers the REST paths sandboxd would. ----------

interface FakeRouter {
  port: number;
  headers: http.IncomingHttpHeaders[];
  close(): Promise<void>;
}

function startFakeRouter(): Promise<FakeRouter> {
  const headers: http.IncomingHttpHeaders[] = [];
  const server = http.createServer((req, res) => {
    headers.push(req.headers);
    if (req.url === "/v1/health") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ status: "ok", uptime_seconds: 1 }));
      return;
    }
    if (req.url?.startsWith("/v1/files/")) {
      res.writeHead(200, { "content-type": "application/octet-stream" });
      res.end("file contents");
      return;
    }
    res.writeHead(404);
    res.end();
  });
  return new Promise((resolve, reject) => {
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        reject(new Error("failed to bind fake router"));
        return;
      }
      resolve({
        port: addr.port,
        headers,
        close: () => new Promise((r) => server.close(() => r())),
      });
    });
    server.on("error", reject);
  });
}

// ---------- fake apiserver: EndpointSlices list + a single-port port-forward
// to the router, mirroring how a real apiserver fronts both. ----------

interface FakeApiServer {
  port: number;
  endpointSliceRequests: string[];
  portForwardPods: string[];
  close(): Promise<void>;
}

function startFakeApiServer(
  routerPort: number,
  options?: { releaseEndpointSlice?: Promise<void> },
): Promise<FakeApiServer> {
  const endpointSliceRequests: string[] = [];
  const portForwardPods: string[] = [];
  const sockets = new Set<net.Socket>();

  const httpServer = http.createServer((req, res) => {
    const url = new URL(req.url ?? "", "http://localhost");
    if (
      url.pathname ===
      `/apis/discovery.k8s.io/v1/namespaces/${ROUTER_NAMESPACE}/endpointslices`
    ) {
      endpointSliceRequests.push(req.url ?? "");
      const send = () => {
        if (res.writableEnded || res.destroyed) return;
        res.writeHead(200, { "content-type": "application/json" });
        res.end(
          JSON.stringify({
            apiVersion: "discovery.k8s.io/v1",
            kind: "EndpointSliceList",
            items: [
              {
                apiVersion: "discovery.k8s.io/v1",
                kind: "EndpointSlice",
                addressType: "IPv4",
                endpoints: [
                  {
                    addresses: ["10.244.0.1"],
                    conditions: { ready: true },
                    targetRef: {
                      kind: "Pod",
                      namespace: ROUTER_NAMESPACE,
                      name: ROUTER_POD,
                    },
                  },
                ],
              },
            ],
          }),
        );
      };
      const release = options?.releaseEndpointSlice;
      if (release) {
        release.then(send, send);
      } else {
        send();
      }
      return;
    }
    res.writeHead(404);
    res.end();
  });

  httpServer.on("connection", (socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
  });

  const wss = new WebSocketServer({ noServer: true });
  const openSockets = new Set<WSType>();

  httpServer.on("upgrade", (req, socket, head) => {
    const url = new URL(req.url ?? "", "http://localhost");
    const ports = url.searchParams.get("ports");
    if (ports !== String(8080)) {
      socket.destroy();
      return;
    }
    const pod = url.pathname.match(/\/pods\/([^/]+)\/portforward/)?.[1];
    if (pod) portForwardPods.push(pod);

    wss.handleUpgrade(req, socket, head, (ws) => {
      openSockets.add(ws);
      ws.on("close", () => openSockets.delete(ws));
      // Channel headers on both data (0) and error (1) channels, then the
      // client re-dials one WS per accepted TCP connection.
      ws.send(Buffer.from([0, 0, 0]));
      ws.send(Buffer.from([1, 0, 0]));

      const target = net.connect(routerPort, "127.0.0.1");
      target.on("data", (chunk: Buffer) => {
        if (ws.readyState === ws.OPEN) {
          ws.send(Buffer.concat([Buffer.from([0]), chunk]));
        }
      });
      target.on("close", () => ws.close());
      target.on("error", () => ws.terminate());
      ws.on("message", (raw: Buffer) => {
        if (raw.length > 0 && raw[0] === 0) target.write(raw.subarray(1));
      });
      ws.on("close", () => target.destroy());
      ws.on("error", () => target.destroy());
    });
  });

  return new Promise((resolve, reject) => {
    httpServer.listen(0, "127.0.0.1", () => {
      const addr = httpServer.address();
      if (!addr || typeof addr === "string") {
        reject(new Error("failed to bind fake apiserver"));
        return;
      }
      resolve({
        port: addr.port,
        endpointSliceRequests,
        portForwardPods,
        close: () =>
          new Promise((r) => {
            for (const ws of openSockets) ws.terminate();
            for (const socket of sockets) socket.destroy();
            httpServer.close(() => r());
          }),
      });
    });
    httpServer.on("error", reject);
  });
}

function makeSandbox(
  apiServerPort: number,
  overrides: {
    disablePodIPRouting?: boolean;
    authToken?: string;
    routerAuthToken?: string;
    serviceFQDN?: string;
    portForwardReadyTimeoutMs?: number;
  } = {},
): Sandbox {
  const init: SandboxInit = {
    claimName: "test-claim",
    sandboxName: "sb-kata",
    podName: "sb-kata",
    podIP: "10.244.0.7",
    serviceFQDN: overrides.serviceFQDN,
    namespace: "default",
    customObjectsApi: {
      deleteNamespacedCustomObject: vi.fn().mockResolvedValue({}),
    } as unknown as k8s.CustomObjectsApi,
    kubeConfig: makeTestKubeConfig(apiServerPort),
    sandboxdOptions: normalizeSandboxdOptions({
      connectivity: "router-port-forward",
      restPort: REST_PORT,
      grpcPort: GRPC_PORT,
      routerNamespace: ROUTER_NAMESPACE,
      portForwardReadyTimeoutMs: overrides.portForwardReadyTimeoutMs ?? 5000,
      disablePodIPRouting: overrides.disablePodIPRouting,
      authToken: overrides.authToken,
      routerAuthToken: overrides.routerAuthToken,
    }),
    tracingManager: null,
    traceServiceName: "test",
    logger: noopLogger,
  };
  return new Sandbox(init);
}

function makeTestKubeConfig(apiServerPort: number): k8s.KubeConfig {
  const kc = new k8s.KubeConfig();
  kc.loadFromOptions({
    clusters: [
      {
        name: "c",
        server: `http://127.0.0.1:${apiServerPort}`,
        skipTLSVerify: true,
      },
    ],
    users: [{ name: "u" }],
    contexts: [{ name: "ctx", cluster: "c", user: "u" }],
    currentContext: "ctx",
  });
  return kc;
}

let router: Awaited<ReturnType<typeof startFakeRouter>> | undefined;
let api: Awaited<ReturnType<typeof startFakeApiServer>> | undefined;
let sandbox: Sandbox | undefined;

afterEach(async () => {
  await sandbox?.closeLocal().catch(() => {});
  sandbox = undefined;
  await api?.close();
  api = undefined;
  await router?.close();
  router = undefined;
});

describe("router-port-forward connectivity", () => {
  it("resolves the router pod, shares one port-forward, and routes REST with X-Sandbox-* headers", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port);
    sandbox = makeSandbox(api.port);

    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");

    // Every port-forward dials the router pod (fetch may open >1 TCP conn).
    // Don't assert an exact count.
    expect(api.portForwardPods.length).toBeGreaterThan(0);
    expect(api.portForwardPods.every((p) => p === ROUTER_POD)).toBe(true);
    // The EndpointSlice list was queried once, in the router namespace.
    expect(api.endpointSliceRequests).toHaveLength(1);

    // The lazy connect's health probe and files.read both carried routing
    // headers; X-Sandbox-Port names the sandboxd REST port.
    expect(router.headers.length).toBeGreaterThan(0);
    for (const h of router.headers) {
      expect(h["x-sandbox-id"]).toBe("sb-kata");
      expect(h["x-sandbox-namespace"]).toBe("default");
      expect(h["x-sandbox-port"]).toBe(String(REST_PORT));
      expect(h["x-sandbox-pod-ip"]).toBe("10.244.0.7");
    }
  });

  it("omits X-Sandbox-Pod-IP when disablePodIPRouting is set", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port);
    sandbox = makeSandbox(api.port, { disablePodIPRouting: true });

    await sandbox.files.read("a.txt");

    expect(router.headers.length).toBeGreaterThan(0);
    for (const h of router.headers) {
      expect(h["x-sandbox-id"]).toBe("sb-kata");
      expect(h["x-sandbox-pod-ip"]).toBeUndefined();
    }
  });

  it("omits X-Sandbox-Pod-IP when the Sandbox has a headless Service", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port);
    sandbox = makeSandbox(api.port, {
      serviceFQDN: "sb-kata.default.svc.cluster.local",
    });

    await sandbox.files.read("a.txt");

    expect(router.headers.length).toBeGreaterThan(0);
    for (const h of router.headers) {
      expect(h["x-sandbox-id"]).toBe("sb-kata");
      expect(h["x-sandbox-pod-ip"]).toBeUndefined();
    }
  });

  it("sends the sandboxd token as X-Sandboxd-Token instead of Authorization", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port);
    sandbox = makeSandbox(api.port, { authToken: "tok-sandboxd-9a2f" });

    await sandbox.files.read("a.txt");

    expect(router.headers.length).toBeGreaterThan(0);
    for (const h of router.headers) {
      // The bare token rides X-Sandboxd-Token; Authorization is never sent
      // (the router consumes it for its own credential and strips it).
      expect(h["x-sandboxd-token"]).toBe("tok-sandboxd-9a2f");
      expect(h.authorization).toBeUndefined();
    }
  });

  it("sends the router's own credential as Authorization when routerAuthToken is set", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port);
    sandbox = makeSandbox(api.port, {
      authToken: "tok-sandboxd-9a2f",
      routerAuthToken: "tok-router-7c1e",
    });

    await sandbox.files.read("a.txt");

    expect(router.headers.length).toBeGreaterThan(0);
    for (const h of router.headers) {
      // Two distinct credentials: the router's token as Authorization, and
      // sandboxd's token as X-Sandboxd-Token.
      expect(h.authorization).toBe("Bearer tok-router-7c1e");
      expect(h["x-sandboxd-token"]).toBe("tok-sandboxd-9a2f");
    }
  });

  it("bounds EndpointSlice discovery by the connect deadline and ignores a late list", async () => {
    router = await startFakeRouter();
    let releaseList: () => void = () => {};
    const releaseEndpointSlice = new Promise<void>((resolve) => {
      releaseList = resolve;
    });
    api = await startFakeApiServer(router.port, { releaseEndpointSlice });
    sandbox = makeSandbox(api.port, { portForwardReadyTimeoutMs: 300 });

    const started = Date.now();
    await expect(sandbox.files.read("a.txt")).rejects.toBeInstanceOf(
      SandboxTimeoutError,
    );
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(api.portForwardPods).toEqual([]);

    releaseList();
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(api.portForwardPods).toEqual([]);
  });

  it("closeLocal abandons a stalled EndpointSlice list without opening a tunnel", async () => {
    router = await startFakeRouter();
    api = await startFakeApiServer(router.port, {
      releaseEndpointSlice: new Promise(() => {}),
    });
    sandbox = makeSandbox(api.port, { portForwardReadyTimeoutMs: 30_000 });

    const read = sandbox.files.read("a.txt");
    await vi.waitFor(() => {
      expect(api?.endpointSliceRequests.length).toBeGreaterThan(0);
    });
    const started = Date.now();
    await sandbox.closeLocal();
    expect(Date.now() - started).toBeLessThan(1_000);
    await expect(read).rejects.toBeInstanceOf(SandboxClosedError);
    expect(api.portForwardPods).toEqual([]);
  }, 4_000);
});
