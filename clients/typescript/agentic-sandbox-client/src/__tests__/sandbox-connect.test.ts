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
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import * as k8s from "@kubernetes/client-node";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WebSocket as WSType } from "ws";
import { WebSocketServer } from "ws";
import {
  ExecuteResponseSchema,
  InitEventSchema,
  type ProcessConfig,
  ProcessService,
  type ResizeTTYRequest,
  type SendSignalRequest,
  type StartRequest,
  StartResponseSchema,
  type WriteStdinRequest,
  WriteStdinResponseSchema,
} from "../_proto/process/v1/process_pb.js";
import {
  SandboxClosedError,
  SandboxConnectionError,
  SandboxdRpcError,
  SandboxError,
  SandboxTimeoutError,
} from "../exceptions.js";
import { noopLogger } from "../logger.js";
import type { SandboxInit } from "../sandbox.js";
import { normalizeSandboxdOptions, Sandbox } from "../sandbox.js";
import type { Span, Tracer, TracerManager } from "../trace-manager.js";
import type { ProcessEvent, SandboxdConnectivity } from "../types.js";

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

async function startRestBackend(listenPort = 0): Promise<{
  port: number;
  healthHits: number;
  putBodies: Buffer[];
  close(): Promise<void>;
}> {
  const counter = { healthHits: 0 };
  const putBodies: Buffer[] = [];
  const server = http.createServer((req, res) => {
    if (req.url === "/v1/health") {
      counter.healthHits++;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "ok", uptime_seconds: 1 }));
      return;
    }
    if (req.url === "/v1/metadata") {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ env: { SANDBOX_ID: "sb-1" } }));
      return;
    }
    if (req.url?.endsWith("kill-me.txt")) {
      // Simulates a transport-level failure mid-request (e.g. ECONNRESET):
      // destroy the raw socket without ever sending a response.
      req.socket.destroy();
      return;
    }
    if (req.url?.endsWith("stream-slow.txt")) {
      // Sends one chunk of a streaming download, then hangs — left open so
      // a test can exercise timeout/cancel/in-flight behavior mid-stream,
      // whether or not the consumer keeps reading.
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("first-chunk"));
      return;
    }
    if (req.url?.endsWith("slow.txt")) {
      // Never responds — left open so a test can abort the caller-side
      // signal while the request is still in flight.
      return;
    }
    if (req.url?.endsWith("reset-mid-stream.txt")) {
      // Sends headers plus a first chunk, then resets the connection —
      // simulates a transport-level failure partway through a download.
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      setTimeout(() => req.socket.destroy(), 20);
      return;
    }
    if (req.method === "PUT" && req.url?.startsWith("/v1/files/")) {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        putBodies.push(Buffer.concat(chunks));
        res.writeHead(204);
        res.end();
      });
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
  await new Promise<void>((resolve) =>
    server.listen(listenPort, "127.0.0.1", resolve),
  );
  const addr = server.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind REST backend");
  return {
    port: addr.port,
    get healthHits() {
      return counter.healthHits;
    },
    putBodies,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

async function startGrpcBackend(
  routes: (router: ConnectRouter) => void,
  opts?: { disruptFirstExecute?: "session" | "socket" },
): Promise<{ port: number; close(): Promise<void> }> {
  const inner = connectNodeAdapter({ routes });
  let triggered = false;
  let lastRawSocket: net.Socket | undefined;
  const server = http2.createServer((req, res) => {
    if (
      !triggered &&
      opts?.disruptFirstExecute &&
      req.url === "/process.v1.ProcessService/Execute"
    ) {
      triggered = true;
      if (opts.disruptFirstExecute === "session") {
        // Simulates the server tearing down the HTTP/2 session mid-call
        // (e.g. sandboxd shutting down while Execute is in flight).
        req.stream.session?.destroy();
      } else {
        // req.stream.session.socket is a Proxy that throws
        // ERR_HTTP2_NO_SOCKET_MANIPULATION on destroy(); use the raw socket
        // captured via the server's "connection" event, and
        // resetAndDestroy() so the client sees an actual ECONNRESET.
        lastRawSocket?.resetAndDestroy();
      }
      return;
    }
    inner(req, res);
  });
  server.on("connection", (socket: net.Socket) => {
    lastRawSocket = socket;
  });
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
  connectivity?: SandboxdConnectivity;
  podIP?: string;
  serviceFQDN?: string;
}): Sandbox {
  const init: SandboxInit = {
    claimName: "test-claim",
    sandboxName: "test-sandbox",
    podName: "test-pod",
    podIP: overrides.podIP,
    serviceFQDN: overrides.serviceFQDN,
    namespace: "default",
    customObjectsApi: {
      deleteNamespacedCustomObject:
        overrides.deleteNamespacedCustomObject ?? vi.fn().mockResolvedValue({}),
    } as unknown as k8s.CustomObjectsApi,
    kubeConfig: makeTestKubeConfig(overrides.apiServerPort),
    sandboxdOptions: normalizeSandboxdOptions({
      connectivity: overrides.connectivity,
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

    const result = await sandbox.commands.run("echo", ["ok"]);
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

  it("invalidates the generation on a gRPC session teardown, aborts a concurrent REST call, and reconnects on the next call", async () => {
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend(
      (router) => {
        router.service(ProcessService, {
          execute: () =>
            create(ExecuteResponseSchema, {
              exitCode: 0,
              stdout: new Uint8Array(),
              stderr: new Uint8Array(),
            }),
        });
      },
      { disruptFirstExecute: "session" },
    );
    api = await startFakeApiServer({
      18080: restBackend.port,
      19090: grpcBackend.port,
    });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    // Establishes the first generation.
    await sandbox.files.read("a.txt");
    expect(restBackend.healthHits).toBe(1);
    const firstGenerationConnections = api.connectionsByRequestedPort[18080];

    // A concurrent REST call sharing the same generation, left pending.
    const concurrentRead = sandbox.files.read("slow.txt");

    // Mid-call gRPC session teardown: must surface as a connection error and
    // invalidate the generation, not hang or get reported as an RPC
    // application error.
    await expect(sandbox.commands.run("echo", ["hi"])).rejects.toBeInstanceOf(
      SandboxConnectionError,
    );

    // The concurrent REST call sharing the invalidated generation is aborted
    // as a bystander of that failure, not left hanging.
    await expect(concurrentRead).rejects.toMatchObject({
      kind: "protocol",
    } satisfies Partial<SandboxConnectionError>);

    // The next call reconnects from scratch: a fresh health check, a new
    // port-forward connection, and a new gRPC session.
    const result = await sandbox.commands.run("echo", ["ok"]);
    expect(result.exitCode).toBe(0);
    expect(restBackend.healthHits).toBe(2);
    expect(api.connectionsByRequestedPort[18080]).toBeGreaterThan(
      firstGenerationConnections,
    );
    expect(api.connectionsByRequestedPort[19090]).toBe(2);
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

describe("Sandbox health() and metadata()", () => {
  it("lazily connects, then probes /v1/health once more and reads /v1/metadata on the same generation", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    await expect(sandbox.metadata()).resolves.toEqual({
      env: { SANDBOX_ID: "sb-1" },
    });
    // The lazy connect's own health check.
    expect(restBackend.healthHits).toBe(1);

    await expect(sandbox.health()).resolves.toEqual({
      status: "ok",
      uptimeSeconds: 1,
    });
    // health() is a real probe, not a cached connect result.
    expect(restBackend.healthHits).toBe(2);
  });

  it("rejects an invalid timeoutMs before connecting", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    await expect(sandbox.metadata({ timeoutMs: 0 })).rejects.toThrow();
    await expect(sandbox.health({ timeoutMs: -1 })).rejects.toThrow();
    expect(restBackend.healthHits).toBe(0);
  });

  it("rejects once closing has started", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });
    await sandbox.closeLocal();
    await expect(sandbox.health()).rejects.toThrow();
    await expect(sandbox.metadata()).rejects.toThrow();
  });
});

describe("Sandbox commands.run() arguments", () => {
  async function startRecordingSandbox(
    tracingManager?: TracerManager,
  ): Promise<ProcessConfig[]> {
    const received: ProcessConfig[] = [];
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend((router) => {
      router.service(ProcessService, {
        execute: (req) => {
          if (req.config) received.push(req.config);
          return create(ExecuteResponseSchema, { exitCode: 0 });
        },
      });
    });
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort: restBackend.port,
      grpcPort: grpcBackend.port,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
      tracingManager,
    });
    return received;
  }

  it("accepts (command), (command, args, opts), and (command, opts)", async () => {
    const received = await startRecordingSandbox();
    const s = sandbox as Sandbox;

    await s.commands.run("pwd");
    await s.commands.run("echo", ["a b", "c"], { env: { FOO: "bar" } });
    await s.commands.run("ls", { cwd: "work" });

    expect(received.map((c) => c.command)).toEqual([
      ["pwd"],
      ["echo", "a b", "c"],
      ["ls"],
    ]);
    expect(received.map((c) => c.envVars)).toEqual([{}, { FOO: "bar" }, {}]);
    expect(received.map((c) => c.cwd)).toEqual([undefined, undefined, "work"]);
  });

  it("keeps the options of (command, undefined, opts)", async () => {
    const received = await startRecordingSandbox();

    await (sandbox as Sandbox).commands.run("pwd", undefined, {
      cwd: "work",
      env: { FOO: "bar" },
    });

    expect(received).toHaveLength(1);
    expect(received[0]?.command).toEqual(["pwd"]);
    expect(received[0]?.cwd).toBe("work");
    expect(received[0]?.envVars).toEqual({ FOO: "bar" });
  });

  it("honors timeoutMs and signal passed as (command, undefined, opts)", async () => {
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend((router) => {
      router.service(ProcessService, {
        // Never completes on its own, so only the caller's timeout or
        // signal can end the call.
        execute: (_req, ctx) =>
          new Promise((_, reject) => {
            ctx.signal.addEventListener("abort", () =>
              reject(ctx.signal.reason),
            );
          }),
      });
    });
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort: restBackend.port,
      grpcPort: grpcBackend.port,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
    });
    const s = sandbox as Sandbox;

    await expect(
      s.commands.run("sleep", undefined, { timeoutMs: 300 }),
    ).rejects.toBeInstanceOf(SandboxTimeoutError);

    const controller = new AbortController();
    const reason = new Error("caller-supplied cancellation");
    setTimeout(() => controller.abort(reason), 50);
    await expect(
      s.commands.run("sleep", undefined, {
        timeoutMs: 30_000,
        signal: controller.signal,
      }),
    ).rejects.toBe(reason);
  });

  it.each([
    ["an empty command", () => (sandbox as Sandbox).commands.run("")],
    [
      "a non-string arg",
      () => (sandbox as Sandbox).commands.run("echo", [1 as unknown as string]),
    ],
    [
      "an env key containing '='",
      () => (sandbox as Sandbox).commands.run("env", { env: { "A=B": "c" } }),
    ],
    [
      "a non-string env value",
      () =>
        (sandbox as Sandbox).commands.run("env", {
          env: { A: 1 as unknown as string },
        }),
    ],
  ])("rejects %s with invalid_argument before sending anything", async (_, call) => {
    const received = await startRecordingSandbox();
    await expect(call()).rejects.toSatisfy(
      (err: unknown) =>
        err instanceof SandboxError && err.telemetryCode === "invalid_argument",
    );
    expect(received).toEqual([]);
    expect(restBackend?.healthHits).toBe(0);
  });

  it("records only the executable's base name on the span", async () => {
    const { tracingManager, events } = makeRecordingTracerManager();
    await startRecordingSandbox(tracingManager);
    await (sandbox as Sandbox).commands.run("/usr/bin/env", ["true"]);
    expect(events).toContain("attr:sandbox.command.executable=env");
    for (const event of events) {
      expect(event).not.toContain("/usr/bin");
    }
  });
});

// An apiserver port nothing listens on: in-cluster connectivity must never
// dial it, so any accidental port-forward attempt fails the test.
const UNUSED_API_SERVER_PORT = 1;

async function reserveFreePort(): Promise<number> {
  const server = net.createServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("failed to bind");
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return addr.port;
}

describe("Sandbox in-cluster connectivity", () => {
  it.each([
    ["in-cluster-pod-ip", { podIP: "127.0.0.1" }],
    ["in-cluster-service", { serviceFQDN: "localhost" }],
  ] as const)("%s dials sandboxd directly for files and commands", async (connectivity, addr) => {
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend((router) => {
      router.service(ProcessService, {
        execute: () =>
          create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new TextEncoder().encode("direct\n"),
            stderr: new Uint8Array(),
          }),
      });
    });
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort: restBackend.port,
      grpcPort: grpcBackend.port,
      connectivity,
      ...addr,
    });

    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    const result = await sandbox.commands.run("echo", ["direct"]);
    expect(result).toEqual({ exitCode: 0, stdout: "direct\n", stderr: "" });
    expect(restBackend.healthHits).toBe(1);
  });

  it("keeps polling health while sandboxd is not listening yet, within the connect budget", async () => {
    const restPort = await reserveFreePort();
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort,
      grpcPort: 1,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
    });

    const pending = sandbox.files.read("a.txt");
    // First health probes hit ECONNREFUSED; sandboxd comes up afterwards.
    await new Promise((resolve) => setTimeout(resolve, 500));
    restBackend = await startRestBackend(restPort);

    const content = await pending;
    expect(new TextDecoder().decode(content)).toBe("file contents");
  });

  it("gives up with a timeout, not a fail-fast error, when sandboxd never comes up", async () => {
    const restPort = await reserveFreePort();
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort,
      grpcPort: 1,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
    });

    await expect(
      sandbox.files.read("a.txt", { timeoutMs: 800 }),
    ).rejects.toBeInstanceOf(SandboxTimeoutError);
  });

  it("invalidates the generation on a transport failure and reconnects on the next call", async () => {
    restBackend = await startRestBackend();
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort: restBackend.port,
      grpcPort: 1,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
    });

    await sandbox.files.read("a.txt");
    expect(restBackend.healthHits).toBe(1);

    await expect(sandbox.files.read("kill-me.txt")).rejects.toBeInstanceOf(
      SandboxConnectionError,
    );

    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    expect(restBackend.healthHits).toBe(2);
  });
});

describe("Sandbox streaming integration", () => {
  it("readStream errors via the per-call timeout without waiting for another read, and releases in-flight promptly", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stream = await sandbox.files.readStream("stream-slow.txt", {
      timeoutMs: 150,
    });
    // Never read from it — the timeout must still surface via `closed`
    // without this test issuing another read() first.
    const reader = stream.getReader();
    await expect(reader.closed).rejects.toBeInstanceOf(SandboxTimeoutError);

    // In-flight was released by the timeout itself, not by close()'s much
    // longer cleanup window — the next call proceeds immediately.
    const startedAt = Date.now();
    await sandbox.files.exists("a.txt");
    expect(Date.now() - startedAt).toBeLessThan(2000);
  });

  it("closeLocal() immediately fails a never-read readStream via lifecycle abort and releases in-flight", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stream = await sandbox.files.readStream("stream-slow.txt");
    const reader = stream.getReader();

    const startedAt = Date.now();
    await sandbox.closeLocal();
    expect(Date.now() - startedAt).toBeLessThan(1000);
    await expect(reader.closed).rejects.toBeInstanceOf(SandboxClosedError);
  });

  it("invalidates the generation on a transport failure mid-readStream and reconnects on the next call", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stream = await sandbox.files.readStream("reset-mid-stream.txt");
    expect(restBackend.healthHits).toBe(1);
    const reader = stream.getReader();
    await reader.read();
    await expect(reader.read()).rejects.toBeInstanceOf(SandboxConnectionError);

    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    expect(restBackend.healthHits).toBe(2);
  });

  it("treats a consumer cancel of readStream as a normal termination: no error code, no size, generation stays valid", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const { tracingManager, events } = makeRecordingTracerManager();
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      tracingManager,
    });

    const stream = await sandbox.files.readStream("stream-slow.txt");
    expect(restBackend.healthHits).toBe(1);
    const reader = stream.getReader();
    await reader.read();
    await reader.cancel();

    expect(events.some((e) => e.startsWith("attr:sandbox.error.code"))).toBe(
      false,
    );
    expect(events.some((e) => e.startsWith("attr:sandbox.file.size"))).toBe(
      false,
    );
    expect(events.some((e) => e.startsWith("status:"))).toBe(false);

    // Generation stays valid: the next call reuses it, no new health check.
    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    expect(restBackend.healthHits).toBe(1);
  });

  it("records sandbox.file.size once a readStream is fully consumed", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const { tracingManager, events } = makeRecordingTracerManager();
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      tracingManager,
    });

    const stream = await sandbox.files.readStream("a.txt");
    const reader = stream.getReader();
    let total = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
    }
    expect(total).toBe("file contents".length);
    // Setting the span attribute happens on a separate promise chain from
    // the stream closing (see readStreamImpl()'s withSpan fn vs. the public
    // wrapper's pull()) — both stem from the same terminate() call, but
    // aren't ordered relative to each other, so poll briefly rather than
    // assume the event is visible the instant the stream reports done.
    await vi.waitFor(() => {
      expect(events).toContain(`attr:sandbox.file.size=${total}`);
    });
  });

  it("never records a secret sentinel for a failed readStream", async () => {
    const SENTINEL = "sekrit-sentinel-readstream-9f3c";
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
      sandbox.files.readStream(`${SENTINEL}/kill-me.txt`),
    ).rejects.toBeTruthy();

    expect(events.length).toBeGreaterThan(0);
    for (const event of events) {
      expect(event).not.toContain(SENTINEL);
    }
  });

  it("writeStream sends the exact content and reuses the generation for the next call", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    const { tracingManager, events } = makeRecordingTracerManager();
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
      tracingManager,
    });

    const encoder = new TextEncoder();
    const chunks = [encoder.encode("hello "), encoder.encode("world")];
    let i = 0;
    const content = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (i >= chunks.length) {
          controller.close();
          return;
        }
        controller.enqueue(chunks[i++]);
      },
    });

    await sandbox.files.writeStream("out.txt", content);
    expect(restBackend.putBodies).toHaveLength(1);
    expect(restBackend.putBodies[0].toString()).toBe("hello world");
    expect(events).toContain("attr:sandbox.file.size=11");
    expect(restBackend.healthHits).toBe(1);

    // Generation stays valid: the next call reuses it, no new health check.
    await sandbox.files.exists("a.txt");
    expect(restBackend.healthHits).toBe(1);
  });

  it("writeStream rejects with the source's own thrown value and keeps the generation valid", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const boom = new Error("source blew up");
    const content = new ReadableStream<Uint8Array>({
      pull() {
        throw boom;
      },
    });

    await expect(sandbox.files.writeStream("out.txt", content)).rejects.toBe(
      boom,
    );
    expect(restBackend.healthHits).toBe(1);

    // Generation stays valid: the next call reuses it, no new health check.
    await sandbox.files.exists("a.txt");
    expect(restBackend.healthHits).toBe(1);
  });

  it("writeStream rejects with a SandboxConnectionError thrown by the source without invalidating the generation", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    // A SandboxConnectionError is normally what invalidates a generation —
    // this one originates in the caller's *source* stream, not the
    // sandboxd connection, so SourceFailure must keep it from doing so
    // (D5): classifyOperationFailure() checks `instanceof SourceFailure`
    // before it ever looks at the wrapped value's type.
    const sourceErr = new SandboxConnectionError(
      "source-side connection error",
      "socket",
    );
    const content = new ReadableStream<Uint8Array>({
      pull() {
        throw sourceErr;
      },
    });

    await expect(sandbox.files.writeStream("out.txt", content)).rejects.toBe(
      sourceErr,
    );
    expect(restBackend.healthHits).toBe(1);

    // Generation stays valid: the next call reuses it, no new health check.
    await sandbox.files.exists("a.txt");
    expect(restBackend.healthHits).toBe(1);
  });

  it("writeStream terminates via the per-call timeout even when the source's pull() never resolves", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stalledContent = new ReadableStream<Uint8Array>({
      pull() {
        return new Promise<void>(() => {
          // never resolves
        });
      },
    });

    const startedAt = Date.now();
    await expect(
      sandbox.files.writeStream("out.txt", stalledContent, {
        timeoutMs: 150,
      }),
    ).rejects.toBeInstanceOf(SandboxTimeoutError);
    expect(Date.now() - startedAt).toBeLessThan(1000);

    // In-flight was released by the timeout, not left hanging on the stuck
    // source — the next call proceeds on the same generation.
    await sandbox.files.exists("a.txt");
    expect(restBackend.healthHits).toBe(1);
  });

  it("writeStream terminates via the caller's own AbortSignal even when the source's pull() never resolves", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stalledContent = new ReadableStream<Uint8Array>({
      pull() {
        return new Promise<void>(() => {
          // never resolves
        });
      },
    });
    const controller = new AbortController();
    const reason = new Error("caller cancel");
    setTimeout(() => controller.abort(reason), 50);

    const startedAt = Date.now();
    await expect(
      sandbox.files.writeStream("out.txt", stalledContent, {
        signal: controller.signal,
      }),
    ).rejects.toBe(reason);
    expect(Date.now() - startedAt).toBeLessThan(1000);

    await sandbox.files.exists("a.txt");
    expect(restBackend.healthHits).toBe(1);
  });

  it("a concurrent transport failure on another call invalidates the generation for an in-flight readStream too", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const stream = await sandbox.files.readStream("stream-slow.txt");
    expect(restBackend.healthHits).toBe(1);
    const reader = stream.getReader();
    await reader.read(); // consumes the one available chunk; stream now stalled

    // A concurrent operation's transport failure invalidates the whole
    // generation — including this still-open readStream.
    await expect(sandbox.files.read("kill-me.txt")).rejects.toBeTruthy();
    await expect(reader.closed).rejects.toMatchObject({ kind: "protocol" });

    // The next call reconnects from scratch.
    const content = await sandbox.files.read("a.txt");
    expect(new TextDecoder().decode(content)).toBe("file contents");
    expect(restBackend.healthHits).toBe(2);
  });

  it("writeStream rejects an already-locked content stream before connecting", async () => {
    restBackend = await startRestBackend();
    api = await startFakeApiServer({ 18080: restBackend.port, 19090: 1 });
    sandbox = makeSandbox({
      apiServerPort: api.port,
      restPort: 18080,
      grpcPort: 19090,
    });

    const content = new ReadableStream<Uint8Array>({ pull() {} });
    content.getReader();

    await expect(sandbox.files.writeStream("out.txt", content)).rejects.toThrow(
      SandboxError,
    );
    expect(restBackend.healthHits).toBe(0);
  });
});

// ---------- commands.start() ----------

interface ProcessServerLog {
  starts: StartRequest[];
  stdin: WriteStdinRequest[];
  signals: SendSignalRequest[];
  resizes: ResizeTTYRequest[];
  /** Start streams whose request context was aborted (client tore them down). */
  aborted: number;
  /** Artificial latency of every WriteStdin call. */
  stdinDelayMs?: number;
}

const START_PID = 7;

/**
 * A fake ProcessService whose Start behavior is picked by `command[0]`:
 * "output" (stdout, stderr, exit 3), "no-exit" (Init, then a clean end),
 * "hang" (Init, then idle until aborted), "chatty-hang" (Init, one stdout
 * chunk, then idle until aborted), "slow-init" (never sends Init),
 * "missing" (NOT_FOUND before Init).
 */
function processRoutes(log: ProcessServerLog): (router: ConnectRouter) => void {
  const encode = (text: string) => new TextEncoder().encode(text);
  return (router) => {
    router.service(ProcessService, {
      async *start(req, ctx) {
        log.starts.push(req);
        const untilAborted = () =>
          new Promise<void>((resolve) => {
            const onAbort = () => {
              log.aborted++;
              resolve();
            };
            if (ctx.signal.aborted) onAbort();
            else ctx.signal.addEventListener("abort", onAbort, { once: true });
          });
        const scenario = req.config?.command[0];
        if (scenario === "missing") {
          throw new ConnectError("no such command", Code.NotFound);
        }
        if (scenario === "slow-init") {
          await untilAborted();
          return;
        }
        yield create(StartResponseSchema, {
          event: {
            case: "init",
            value: create(InitEventSchema, { processId: START_PID }),
          },
        });
        if (scenario === "output") {
          yield create(StartResponseSchema, {
            event: { case: "stdout", value: encode("hello ") },
          });
          yield create(StartResponseSchema, {
            event: { case: "stderr", value: encode("warn") },
          });
          yield create(StartResponseSchema, {
            event: { case: "exit", value: { exitCode: 3 } },
          });
        } else if (scenario === "chatty-hang") {
          yield create(StartResponseSchema, {
            event: { case: "stdout", value: encode("first") },
          });
          await untilAborted();
        } else if (scenario === "hang") {
          await untilAborted();
        }
      },
      writeStdin: async (req) => {
        log.stdin.push(req);
        if (log.stdinDelayMs) {
          await new Promise((r) => setTimeout(r, log.stdinDelayMs));
        }
        return create(WriteStdinResponseSchema, {});
      },
      sendSignal: (req) => {
        log.signals.push(req);
        return {};
      },
      resizeTTY: (req) => {
        log.resizes.push(req);
        return {};
      },
      execute: () => create(ExecuteResponseSchema, { exitCode: 0 }),
    });
  };
}

async function eventually(check: () => boolean, ms = 2000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!check()) {
    if (Date.now() > deadline) throw new Error("condition not met in time");
    await new Promise((r) => setTimeout(r, 10));
  }
}

async function collectEvents(
  events: AsyncIterable<ProcessEvent>,
): Promise<ProcessEvent[]> {
  const out: ProcessEvent[] = [];
  for await (const event of events) out.push(event);
  return out;
}

describe("Sandbox commands.start()", () => {
  let log: ProcessServerLog;

  async function startSandbox(opts?: {
    disruptFirstExecute?: "session" | "socket";
  }): Promise<Sandbox> {
    log = { starts: [], stdin: [], signals: [], resizes: [], aborted: 0 };
    restBackend = await startRestBackend();
    grpcBackend = await startGrpcBackend(processRoutes(log), opts);
    sandbox = makeSandbox({
      apiServerPort: UNUSED_API_SERVER_PORT,
      restPort: restBackend.port,
      grpcPort: grpcBackend.port,
      connectivity: "in-cluster-pod-ip",
      podIP: "127.0.0.1",
    });
    return sandbox;
  }

  it("streams stdout/stderr/exit events and passes argv, env, cwd, and pty through", async () => {
    const s = await startSandbox();

    const handle = await s.commands.start("output", ["a b"], {
      env: { FOO: "bar" },
      cwd: "work",
      pty: { cols: 100, rows: 30 },
    });

    expect(handle.pid).toBe(START_PID);
    const events = await collectEvents(handle.events);
    expect(events.map((e) => e.type)).toEqual(["stdout", "stderr", "exit"]);
    const decoder = new TextDecoder();
    expect(events[0]?.type === "stdout" && decoder.decode(events[0].data)).toBe(
      "hello ",
    );
    expect(events[2]).toEqual({ type: "exit", exitCode: 3 });
    await expect(handle.wait()).resolves.toEqual({ exitCode: 3 });

    const config = log.starts[0]?.config;
    expect(config?.command).toEqual(["output", "a b"]);
    expect(config?.envVars).toEqual({ FOO: "bar" });
    expect(config?.cwd).toBe("work");
    expect(log.starts[0]?.pty).toMatchObject({ cols: 100, rows: 30 });
  });

  it("accepts (command) and (command, opts)", async () => {
    const s = await startSandbox();

    await s.commands.start("no-exit");
    await s.commands.start("no-exit", { cwd: "work" });

    expect(log.starts.map((r) => r.config?.command)).toEqual([
      ["no-exit"],
      ["no-exit"],
    ]);
    expect(log.starts[0]?.pty).toBeUndefined();
    expect(log.starts[1]?.config?.cwd).toBe("work");
  });

  it("delivers output to onStdout/onStderr and resolves wait()", async () => {
    const s = await startSandbox();
    const out: string[] = [];
    const err: string[] = [];
    const decoder = new TextDecoder();

    const handle = await s.commands.start("output", {
      onStdout: (chunk) => out.push(decoder.decode(chunk)),
      onStderr: (chunk) => err.push(decoder.decode(chunk)),
    });

    await expect(handle.wait()).resolves.toEqual({ exitCode: 3 });
    expect(out).toEqual(["hello "]);
    expect(err).toEqual(["warn"]);
  });

  it("lets wait() alone drain the stream, and makes the consumers mutually exclusive", async () => {
    const s = await startSandbox();

    const viaWait = await s.commands.start("output");
    await expect(viaWait.wait()).resolves.toEqual({ exitCode: 3 });
    expect(() => viaWait.events).toThrow(/already consumed/);

    const viaCallback = await s.commands.start("output", {
      onStdout: () => {},
    });
    expect(() => viaCallback.events).toThrow(/already consumed/);
    await viaCallback.wait();

    // Iterating first is fine, and wait() then resolves from the iteration.
    const viaEvents = await s.commands.start("output");
    await collectEvents(viaEvents.events);
    await expect(viaEvents.wait()).resolves.toEqual({ exitCode: 3 });
  });

  it("kills the process and rejects wait() with the error a callback threw", async () => {
    const s = await startSandbox();
    const boom = new Error("callback failed");

    const handle = await s.commands.start("output", {
      onStdout: () => {
        throw boom;
      },
    });

    await expect(handle.wait()).rejects.toBe(boom);
    // The handle is closed as well, so the stream is torn down.
    await expect(handle.write("x")).rejects.toBeInstanceOf(SandboxClosedError);
  });

  it("rejects a missing executable with SandboxdRpcError not_found before returning a handle", async () => {
    const s = await startSandbox();

    const err = await s.commands.start("missing").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(SandboxdRpcError);
    expect((err as SandboxdRpcError).code).toBe("not_found");
  });

  it("reports a stream that ends without an ExitEvent as a protocol error", async () => {
    const s = await startSandbox();

    const handle = await s.commands.start("no-exit");

    await expect(collectEvents(handle.events)).rejects.toMatchObject({
      name: "SandboxConnectionError",
      kind: "protocol",
    });
    await expect(handle.wait()).rejects.toBeInstanceOf(SandboxConnectionError);
  });

  it("validates arguments before connecting", async () => {
    const s = await startSandbox();

    await expect(s.commands.start("")).rejects.toMatchObject({
      telemetryCode: "invalid_argument",
    });
    await expect(
      s.commands.start("sh", { pty: { cols: 0, rows: 24 } }),
    ).rejects.toMatchObject({ telemetryCode: "invalid_argument" });
    await expect(
      s.commands.start("sh", { pty: { cols: 80, rows: 70000 } }),
    ).rejects.toMatchObject({ telemetryCode: "invalid_argument" });
    await expect(
      s.commands.start("sh", { onStdout: "nope" as never }),
    ).rejects.toMatchObject({ telemetryCode: "invalid_argument" });
    expect(log.starts).toHaveLength(0);
  });

  it("sends stdin, EOF, signals, and resizes for the process's pid", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("hang");
    const pty = await s.commands.start("hang", { pty: { cols: 80, rows: 24 } });

    await handle.write("hello");
    await handle.closeStdin();
    await handle.signal("SIGTERM");
    await handle.kill();
    await pty.resize({ cols: 120, rows: 40 });

    expect(log.stdin.map((r) => r.processId)).toEqual([START_PID, START_PID]);
    const first = log.stdin[0]?.payload;
    expect(first?.case).toBe("input");
    expect(new TextDecoder().decode(first?.value as Uint8Array)).toBe("hello");
    expect(log.stdin[1]?.payload.case).toBe("eof");
    // SIGTERM = 15, SIGKILL = 9.
    expect(log.signals.map((r) => r.signal)).toEqual([15, 9]);
    expect(log.resizes).toHaveLength(1);
    expect(log.resizes[0]).toMatchObject({
      processId: START_PID,
      cols: 120,
      rows: 40,
    });
  });

  it("splits a large write into ordered chunks", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("hang");
    const data = new Uint8Array(2.5 * 1024 * 1024).map((_, i) => i % 251);

    await handle.write(data);

    expect(
      log.stdin.map((r) => (r.payload.value as Uint8Array).length),
    ).toEqual([1024 * 1024, 1024 * 1024, 0.5 * 1024 * 1024]);
    const joined = Buffer.concat(
      log.stdin.map((r) => r.payload.value as Uint8Array),
    );
    expect(joined.equals(Buffer.from(data))).toBe(true);
  });

  it("shares one timeoutMs budget across the chunks of a large write", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("hang");
    // Three chunks at ~300ms each: every RPC fits in 700ms on its own (so
    // per-chunk timers would let the whole write succeed after ~900ms), but
    // the call as a whole does not.
    log.stdinDelayMs = 300;

    const startedAt = Date.now();
    await expect(
      handle.write(new Uint8Array(2.5 * 1024 * 1024), { timeoutMs: 700 }),
    ).rejects.toBeInstanceOf(SandboxTimeoutError);

    expect(Date.now() - startedAt).toBeLessThan(850);
  });

  it("rejects bad control calls client-side, including closeStdin() on a PTY", async () => {
    const s = await startSandbox();
    const plain = await s.commands.start("hang");
    const pty = await s.commands.start("hang", { pty: { cols: 80, rows: 24 } });

    await expect(pty.closeStdin()).rejects.toMatchObject({
      telemetryCode: "invalid_argument",
    });
    await expect(plain.signal("SIGHUP" as never)).rejects.toMatchObject({
      telemetryCode: "invalid_argument",
    });
    await expect(plain.resize({ cols: 80, rows: 0 })).rejects.toMatchObject({
      telemetryCode: "invalid_argument",
    });
    expect(log.stdin).toHaveLength(0);
    expect(log.signals).toHaveLength(0);
    expect(log.resizes).toHaveLength(0);
  });

  it("handle.close() tears the stream down and leaves the connection generation valid", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("hang");
    const pending = collectEvents(handle.events);
    expect(restBackend?.healthHits).toBe(1);

    await handle.close();

    await expect(pending).rejects.toBeInstanceOf(SandboxClosedError);
    await eventually(() => log.aborted === 1);
    await expect(handle.write("x")).rejects.toBeInstanceOf(SandboxClosedError);
    await expect(handle.wait()).rejects.toBeInstanceOf(SandboxClosedError);
    // The shared connection was not invalidated by closing one process:
    // REST and gRPC both keep working without a reconnect.
    await s.files.read("a.txt");
    await s.commands.run("echo");
    expect(restBackend?.healthHits).toBe(1);
  });

  it("bounds only the startup with timeoutMs, not the process's lifetime", async () => {
    const s = await startSandbox();

    await expect(
      s.commands.start("slow-init", { timeoutMs: 150 }),
    ).rejects.toBeInstanceOf(SandboxTimeoutError);
    await eventually(() => log.aborted === 1);

    const handle = await s.commands.start("hang", { timeoutMs: 150 });
    await new Promise((r) => setTimeout(r, 400));
    await handle.write("still alive");
    expect(log.aborted).toBe(1);
  });

  it("aborting the caller's signal after start kills the stream with the caller's reason", async () => {
    const s = await startSandbox();
    const controller = new AbortController();
    const reason = new Error("stop it");
    const handle = await s.commands.start("hang", {
      signal: controller.signal,
    });
    const waiting = handle.wait();

    controller.abort(reason);

    await expect(waiting).rejects.toBe(reason);
    await eventually(() => log.aborted === 1);
  });

  it("settles wait() on cancellation even after the caller stopped iterating events", async () => {
    const s = await startSandbox();
    const controller = new AbortController();
    const reason = new Error("stop it");
    const handle = await s.commands.start("chatty-hang", {
      signal: controller.signal,
    });
    for await (const _event of handle.events) break;
    const waiting = handle.wait();

    controller.abort(reason);

    await expect(waiting).rejects.toBe(reason);
  });

  it("settles wait() when the Sandbox or the handle is closed after the caller stopped iterating", async () => {
    const s = await startSandbox();
    const viaHandle = await s.commands.start("chatty-hang");
    for await (const _event of viaHandle.events) break;
    const viaSandbox = await s.commands.start("chatty-hang");
    for await (const _event of viaSandbox.events) break;

    await viaHandle.close();
    await expect(viaHandle.wait()).rejects.toBeInstanceOf(SandboxClosedError);

    const waiting = viaSandbox.wait();
    await s.closeLocal();
    await expect(waiting).rejects.toBeInstanceOf(SandboxClosedError);
  });

  it("keeps a clean exit resolved when the Sandbox is closed afterwards", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("output");
    await collectEvents(handle.events);

    await s.closeLocal();

    await expect(handle.wait()).resolves.toEqual({ exitCode: 3 });
  });

  it("does not hold up close() for a long-lived process, and ends it with SandboxClosedError", async () => {
    const s = await startSandbox();
    const handle = await s.commands.start("hang");
    const waiting = handle.wait();

    const startedAt = Date.now();
    await s.close();

    expect(Date.now() - startedAt).toBeLessThan(2000);
    await expect(waiting).rejects.toBeInstanceOf(SandboxClosedError);
    await eventually(() => log.aborted === 1);
    await expect(handle.write("x")).rejects.toBeInstanceOf(SandboxClosedError);
  });

  it("fails control calls with SandboxConnectionError once the process's connection was replaced", async () => {
    const s = await startSandbox({ disruptFirstExecute: "session" });
    const handle = await s.commands.start("hang");
    const waiting = handle.wait();
    waiting.catch(() => {});

    // Tearing down the gRPC session under a concurrent call invalidates the
    // whole generation, including the process's stream.
    await expect(s.commands.run("echo")).rejects.toBeInstanceOf(
      SandboxConnectionError,
    );
    await expect(waiting).rejects.toBeInstanceOf(SandboxConnectionError);

    // Even after the next call reconnects, the old pid is not addressable.
    await s.commands.run("echo");
    await expect(handle.write("x")).rejects.toBeInstanceOf(
      SandboxConnectionError,
    );
    expect(log.stdin).toHaveLength(0);
  });
});
