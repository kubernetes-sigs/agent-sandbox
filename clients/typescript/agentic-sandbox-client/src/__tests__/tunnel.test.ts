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
import { afterEach, describe, expect, it } from "vitest";
import type { WebSocket as WSType } from "ws";
import { WebSocketServer } from "ws";
import { noopLogger } from "../logger.js";
import { PodTunnel } from "../tunnel.js";

// ---------- fake apiserver: speaks just enough of the k8s portforward
// sub-protocol (channel-framed, 2-byte port header per channel) to exercise
// PodTunnel's client-side framing/backpressure/lifecycle behavior. ----------

interface FakeApiServerOptions {
  targetPort: number;
  handshakeDelayMs?: number;
  sendErrorPayload?: Buffer;
  sendDataPayload?: Buffer;
}

async function startFakeApiServer(
  opts: FakeApiServerOptions,
): Promise<{ port: number; close(): Promise<void> }> {
  const httpServer = http.createServer();
  const wss = new WebSocketServer({ noServer: true });
  const openSockets = new Set<WSType>();

  const handleConnection = (ws: WSType) => {
    openSockets.add(ws);
    ws.on("close", () => openSockets.delete(ws));
    // Header-only frames (channel byte + 2-byte port number) on both
    // channels — normal per the port-forward sub-protocol, not real data.
    ws.send(Buffer.from([0, 0, 0]));
    ws.send(Buffer.from([1, 0, 0]));

    if (opts.sendErrorPayload) {
      ws.send(Buffer.concat([Buffer.from([1]), opts.sendErrorPayload]));
      return;
    }

    if (opts.sendDataPayload) {
      // Sent as a single WS message immediately followed by a clean close,
      // so the close frame rides the same TCP read as the message tail —
      // this is what reproduces the client's "message" + "close" landing in
      // the same tick (see the regression test below).
      ws.send(Buffer.concat([Buffer.from([0]), opts.sendDataPayload]));
      ws.close();
      return;
    }

    const target = net.connect(opts.targetPort, "127.0.0.1");
    target.on("data", (chunk: Buffer) => {
      if (ws.readyState === ws.OPEN) {
        ws.send(Buffer.concat([Buffer.from([0]), chunk]));
      }
    });
    target.on("close", () => ws.close());
    target.on("error", () => ws.terminate());
    ws.on("message", (raw: Buffer) => {
      if (raw.length > 0 && raw[0] === 0) {
        target.write(raw.subarray(1));
      }
    });
    ws.on("close", () => target.destroy());
    ws.on("error", () => target.destroy());
  };

  httpServer.on("upgrade", (req, socket, head) => {
    const doUpgrade = () => {
      wss.handleUpgrade(req, socket, head, handleConnection);
    };
    if (opts.handshakeDelayMs) {
      setTimeout(doUpgrade, opts.handshakeDelayMs);
    } else {
      doUpgrade();
    }
  });

  await new Promise<void>((resolve) =>
    httpServer.listen(0, "127.0.0.1", resolve),
  );
  const addr = httpServer.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind fake apiserver");
  return {
    port: addr.port,
    close: () =>
      new Promise((resolve) => {
        for (const ws of openSockets) ws.terminate();
        httpServer.close(() => resolve());
      }),
  };
}

async function startEchoServer(): Promise<{
  port: number;
  close(): Promise<void>;
}> {
  const server = net.createServer((socket) => socket.pipe(socket));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string")
    throw new Error("failed to bind echo server");
  return {
    port: addr.port,
    close: () => new Promise((resolve) => server.close(() => resolve())),
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

function connectClient(port: number): Promise<net.Socket> {
  return new Promise((resolve, reject) => {
    const socket = net.connect(port, "127.0.0.1");
    socket.once("connect", () => resolve(socket));
    socket.once("error", reject);
  });
}

// ---------- lifecycle tracked across tests for cleanup ----------

let api: Awaited<ReturnType<typeof startFakeApiServer>> | undefined;
let echo: Awaited<ReturnType<typeof startEchoServer>> | undefined;
let tunnel: PodTunnel | undefined;

afterEach(async () => {
  await tunnel?.close();
  tunnel = undefined;
  await api?.close();
  api = undefined;
  await echo?.close();
  echo = undefined;
});

describe("PodTunnel", () => {
  it("relays bytes bidirectionally through the REST listener, stripping the port header", async () => {
    echo = await startEchoServer();
    api = await startFakeApiServer({ targetPort: echo.port });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));

    const received = new Promise<Buffer>((resolve) =>
      client.once("data", resolve),
    );
    client.write("hello sandboxd");
    expect((await received).toString()).toBe("hello sandboxd");
    client.destroy();
  });

  it("preserves byte integrity for a payload larger than the backpressure threshold", async () => {
    echo = await startEchoServer();
    api = await startFakeApiServer({ targetPort: echo.port });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));

    const payload = Buffer.alloc(2 * 1024 * 1024);
    for (let i = 0; i < payload.length; i++) payload[i] = i % 256;

    const chunks: Buffer[] = [];
    let total = 0;
    const done = new Promise<void>((resolve) => {
      client.on("data", (chunk: Buffer) => {
        chunks.push(chunk);
        total += chunk.length;
        if (total >= payload.length) resolve();
      });
    });
    client.write(payload);
    await done;
    expect(Buffer.concat(chunks).subarray(0, payload.length)).toEqual(payload);
    client.destroy();
  });

  it("flushes data already queued for a slow local reader after the WS closes normally", async () => {
    const payload = Buffer.alloc(8 * 1024 * 1024);
    for (let i = 0; i < payload.length; i++) payload[i] = i % 256;

    api = await startFakeApiServer({ targetPort: 1, sendDataPayload: payload });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));
    // Slow reader: nothing drains the tunnel's local socket until we
    // resume() below, well after the apiserver has sent the payload and
    // closed normally.
    client.pause();

    let sawError = false;
    client.once("error", () => {
      sawError = true;
    });
    const chunks: Buffer[] = [];
    client.on("data", (chunk: Buffer) => chunks.push(chunk));
    const ended = new Promise<void>((resolve) => client.once("end", resolve));

    await new Promise((resolve) => setTimeout(resolve, 100));
    client.resume();
    await ended;

    expect(sawError).toBe(false);
    // vitest's deep-equality diffing is prohibitively slow on multi-MiB
    // Buffers; Buffer#equals() does a native byte-for-byte compare instead.
    expect(Buffer.concat(chunks).equals(payload)).toBe(true);
  });

  it("tears down the pair on a non-empty error-channel payload", async () => {
    api = await startFakeApiServer({
      targetPort: 1,
      sendErrorPayload: Buffer.from("port-forward failed"),
    });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));

    await new Promise<void>((resolve) => client.once("close", () => resolve()));
  });

  it("destroys the local socket when the WS handshake exceeds handshakeTimeoutMs", async () => {
    api = await startFakeApiServer({ targetPort: 1, handshakeDelayMs: 2000 });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 100,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));

    await new Promise<void>((resolve) => client.once("close", () => resolve()));
  }, 10_000);

  it("terminates established pairs on close()", async () => {
    echo = await startEchoServer();
    api = await startFakeApiServer({ targetPort: echo.port });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    const url = new URL(endpoints.restBaseUrl);
    const client = await connectClient(Number(url.port));
    // Give the pairing a moment to finish establishing before closing.
    await new Promise((resolve) => setTimeout(resolve, 50));

    const closed = new Promise<void>((resolve) =>
      client.once("close", () => resolve()),
    );
    await tunnel.close();
    await closed;
  });

  it("exposes independent REST and gRPC local ports", async () => {
    echo = await startEchoServer();
    api = await startFakeApiServer({ targetPort: echo.port });
    tunnel = new PodTunnel({
      kubeConfig: makeTestKubeConfig(api.port),
      namespace: "ns1",
      podName: "pod1",
      restTargetPort: 8080,
      grpcTargetPort: 9090,
      handshakeTimeoutMs: 5000,
      logger: noopLogger,
    });
    const endpoints = await tunnel.start();
    expect(new URL(endpoints.restBaseUrl).port).not.toBe(
      new URL(endpoints.grpcBaseUrl).port,
    );
  });
});
