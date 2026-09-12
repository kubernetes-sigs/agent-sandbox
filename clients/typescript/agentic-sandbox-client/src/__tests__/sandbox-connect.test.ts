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
import * as http2 from "node:http2";
import * as net from "node:net";
import { create } from "@bufbuild/protobuf";
import type { ConnectRouter } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import * as k8s from "@kubernetes/client-node";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WebSocket as WSType } from "ws";
import { WebSocketServer } from "ws";
import {
  ExecuteResponseSchema,
  ProcessService,
} from "../_proto/process/v1/process_pb.js";
import { noopLogger } from "../logger.js";
import type { SandboxInit } from "../sandbox.js";
import { normalizeSandboxdOptions, Sandbox } from "../sandbox.js";
import type { Span, Tracer, TracerManager } from "../trace-manager.js";

// ---------- fake apiserver: routes by the requested target port to one of
// two real backends (REST / gRPC), mirroring how sandboxd exposes both ports
// on the same Pod. ----------

async function startFakeApiServer(
  targetPortByPort: Record<number, number>,
  rejectPortsWithStatus: Record<number, number> = {},
): Promise<{
  port: number;
  connectionsByRequestedPort: Record<number, number>;
  close(): Promise<void>;
}> {
  const httpServer = http.createServer();
  const wss = new WebSocketServer({ noServer: true });
  const openSockets = new Set<WSType>();
  const connectionsByRequestedPort: Record<number, number> = {};

  const handleConnection = (
    ws: WSType,
    targetPort: number,
    requestedPort: number,
  ) => {
    connectionsByRequestedPort[requestedPort] =
      (connectionsByRequestedPort[requestedPort] ?? 0) + 1;
    openSockets.add(ws);
    ws.on("close", () => openSockets.delete(ws));
    ws.send(Buffer.from([0, 0, 0]));
    ws.send(Buffer.from([1, 0, 0]));

    const target = net.connect(targetPort, "127.0.0.1");
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
  };

  httpServer.on("upgrade", (req, socket, head) => {
    const url = new URL(req.url ?? "", "http://localhost");
    const requestedPort = Number(url.searchParams.get("ports"));
    const rejectStatus = rejectPortsWithStatus[requestedPort];
    if (rejectStatus) {
      // Mirrors a real apiserver denying the port-forward upgrade (auth
      // failure, or the Pod no longer existing): a plain HTTP response
      // instead of a switching-protocols handshake.
      socket.end(`HTTP/1.1 ${rejectStatus} Rejected\r\n\r\n`);
      return;
    }
    const targetPort = targetPortByPort[requestedPort];
    if (!targetPort) {
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) =>
      handleConnection(ws, targetPort, requestedPort),
    );
  });

  await new Promise<void>((resolve) =>
    httpServer.listen(0, "127.0.0.1", resolve),
  );
  const addr = httpServer.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind fake apiserver");
  return {
    port: addr.port,
    connectionsByRequestedPort,
    close: () =>
      new Promise((resolve) => {
        for (const ws of openSockets) ws.terminate();
        httpServer.close(() => resolve());
      }),
  };
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

async function startRestBackend(): Promise<{
  port: number;
  healthHits: number;
  close(): Promise<void>;
}> {
  const counter = { healthHits: 0 };
  const server = http.createServer((req, res) => {
    if (req.url === "/v1/health") {
      counter.healthHits++;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "ok", uptime_seconds: 1 }));
      return;
    }
    if (req.url?.endsWith("kill-me.txt")) {
      // Simulates a transport-level failure mid-request (e.g. ECONNRESET):
      // destroy the raw socket without ever sending a response.
      req.socket.destroy();
      return;
    }
    if (req.url?.endsWith("slow.txt")) {
      // Never responds — left open so a test can abort the caller-side
      // signal while the request is still in flight.
      return;
    }
    if (req.url?.startsWith("/v1/files/")) {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.end("file contents");
      return;
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind REST backend");
  return {
    port: addr.port,
    get healthHits() {
      return counter.healthHits;
    },
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

async function startGrpcBackend(
  routes: (router: ConnectRouter) => void,
): Promise<{ port: number; close(): Promise<void> }> {
  const server = http2.createServer(connectNodeAdapter({ routes }));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind gRPC backend");
  return {
    port: addr.port,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

function makeSandbox(overrides: {
  apiServerPort: number;
  restPort: number;
  grpcPort: number;
  deleteNamespacedCustomObject?: ReturnType<typeof vi.fn>;
  tracingManager?: TracerManager | null;
}): Sandbox {
  const init: SandboxInit = {
    claimName: "test-claim",
    sandboxName: "test-sandbox",
    podName: "test-pod",
    namespace: "default",
    customObjectsApi: {
      deleteNamespacedCustomObject:
        overrides.deleteNamespacedCustomObject ?? vi.fn().mockResolvedValue({}),
    } as unknown as k8s.CustomObjectsApi,
    kubeConfig: makeTestKubeConfig(overrides.apiServerPort),
    sandboxdOptions: normalizeSandboxdOptions({
      restPort: overrides.restPort,
      grpcPort: overrides.grpcPort,
      portForwardReadyTimeoutMs: 5000,
    }),
    tracingManager: overrides.tracingManager ?? null,
    traceServiceName: "test",
    logger: noopLogger,
  };
  return new Sandbox(init);
}

/**
 * A tracer that records every span attribute/status/exception it is given,
 * so a test can assert a sentinel never appears anywhere in tracing output.
 */
function makeRecordingTracerManager(): {
  tracingManager: TracerManager;
  events: string[];
} {
  const events: string[] = [];
  const span: Span = {
    isRecording: () => true,
    setAttribute: (key, value) => {
      events.push(`attr:${key}=${String(value)}`);
    },
    recordException: (exception) => {
      events.push(`exception:${String(exception)}`);
    },
    setStatus: (status) => {
      events.push(`status:${status.message ?? ""}`);
    },
    end: () => {},
  };
  const tracer: Tracer = {
    startSpan: () => span,
    startActiveSpan: (_name, fn) => fn(span),
  };
  const tracingManager = {
    tracer,
    parentContext: null,
    startLifecycleSpan: () => {},
    endLifecycleSpan: () => {},
    getTraceContextJson: () => "",
  } as unknown as TracerManager;
  return { tracingManager, events };
}

// ---------- lifecycle tracking ----------

let api: Awaited<ReturnType<typeof startFakeApiServer>> | undefined;
let restBackend: Awaited<ReturnType<typeof startRestBackend>> | undefined;
let grpcBackend: Awaited<ReturnType<typeof startGrpcBackend>> | undefined;
let sandbox: Sandbox | undefined;

afterEach(async () => {
  await sandbox?.closeLocal().catch(() => {});
  sandbox = undefined;
  await api?.close();
  api = undefined;
  await restBackend?.close();
  restBackend = undefined;
  await grpcBackend?.close();
  grpcBackend = undefined;
});

describe("Sandbox connectivity integration", () => {
  it("lazily connects on first files.read() and reuses the generation for commands.run()", async () => {
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend((router) => {
      router.service(ProcessService, {
        execute: () =>
          create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new TextEncoder().encode("ok\n"),
            stderr: new Uint8Array(),
          }),
      });
    });
    api = await startFakeApiServer({
      18080: restBackend.port,
      19090: grpcBackend.port,
    });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    // The shared connect (tunnel + health check) ran exactly once for the
    // first call. REST's underlying TCP connection count is not asserted
    // here — Node's fetch connection pool may open more than one socket to
    // the same origin, independent of the connection-generation logic.
    expect(restBackend.healthHits).toBe(1);

    const result = await sandbox.commands.run("echo ok");
    expect(result).toEqual({ exitCode: 0, stdout: "ok\n", stderr: "" });
    // gRPC uses a single HTTP/2 session per generation (no connection
    // pooling ambiguity), so the second call must not open a second one.
    expect(api.connectionsByRequestedPort[19090]).toBe(1);
  });

  it("shares a single connect across concurrent first calls (single-flight)", async () => {
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend((router) => {
      router.service(ProcessService, {
        execute: () => create(ExecuteResponseSchema, {}),
      });
    });
    api = await startFakeApiServer({
      18080: restBackend.port,
      19090: grpcBackend.port,
    });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const results = await Promise.all([
      sandbox.files.read("a.txt"),
      sandbox.files.read("b.txt"),
      sandbox.files.exists("c.txt"),
    ]);
    expect(results).toHaveLength(3);
    // The shared connect's health check ran exactly once despite three
    // concurrent first callers (single-flight).
    expect(restBackend.healthHits).toBe(1);
  });

  it("close() drains in-flight work, deletes the claim, and closeLocal()-then-close() still deletes", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const deleteMock = vi.fn().mockResolvedValue({});
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      deleteNamespacedCustomObject: deleteMock,
    });

    await sandbox.files.read("a.txt");
    await sandbox.closeLocal();
    expect(sandbox.isActive).toBe(false);
    expect(deleteMock).not.toHaveBeenCalled();

    await sandbox.close();
    expect(deleteMock).toHaveBeenCalledTimes(1);
  });

  it("rejects new operations once closing has started", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });
    await sandbox.closeLocal();
    await expect(sandbox.files.read("a.txt")).rejects.toThrow();
  });

  it("invalidates the generation on a transport failure and reconnects on the next call", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    // Establishes the first generation.
    await sandbox.files.read("a.txt");
    expect(restBackend.healthHits).toBe(1);
    const firstGenerationConnections = api.connectionsByRequestedPort[18080];

    // Mid-request transport failure: must surface as a connection error and
    // invalidate the generation, not hang or silently succeed.
    await expect(sandbox.files.read("kill-me.txt")).rejects.toBeTruthy();

    // The next call reconnects from scratch: a fresh health check and a new
    // port-forward connection, not a replay against the dead generation.
    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    expect(restBackend.healthHits).toBe(2);
    expect(api.connectionsByRequestedPort[18080]).toBeGreaterThan(
      firstGenerationConnections,
    );
  });

  it("never records a secret sentinel in tracing attributes, status, or exceptions", async () => {
    const SENTINEL = "sekrit-sentinel-9f3c";
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const { tracingManager, events } = makeRecordingTracerManager();
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      tracingManager,
    });

    await expect(
      sandbox.files.read(`${SENTINEL}/kill-me.txt`),
    ).rejects.toBeTruthy();

    expect(events.length).toBeGreaterThan(0);
    for (const event of events) {
      expect(event).not.toContain(SENTINEL);
    }
  });

  it("rejects with the caller's own abort reason and records telemetry code 'cancelled'", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const { tracingManager, events } = makeRecordingTracerManager();
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      tracingManager,
    });

    const controller = new AbortController();
    const reason = new Error("caller-supplied cancellation");
    setTimeout(() => controller.abort(reason), 50);

    await expect(
      sandbox.files.read("slow.txt", { signal: controller.signal }),
    ).rejects.toBe(reason);
    expect(events).toContain("attr:sandbox.error.code=cancelled");
  });

  it("fails fast on a rejected port-forward upgrade instead of retrying for the full timeout", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer(
      { 19090: 1 },
      { 18080: 404 }, // simulates the Pod having disappeared before connect.
    );
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const startedAt = Date.now();
    await expect(sandbox.files.read("a.txt")).rejects.toMatchObject({
      kind: "port_forward",
    });
    // The configured portForwardReadyTimeoutMs is 5000ms; a fail-fast
    // classification must reject well before that, not after exhausting it.
    expect(Date.now() - startedAt).toBeLessThan(2000);
  });
});
