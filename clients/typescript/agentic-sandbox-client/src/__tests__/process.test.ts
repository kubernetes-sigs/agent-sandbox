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

import * as http2 from "node:http2";
import type * as net from "node:net";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { afterEach, describe, expect, it } from "vitest";
import {
  ExecuteResponseSchema,
  ProcessService,
} from "../_proto/process/v1/process_pb.js";
import {
  SandboxClosedError,
  SandboxConnectionError,
  type SandboxdRpcError,
} from "../exceptions.js";
import { ProcessClient } from "../process.js";

let lastReceivedCommand: string[] | undefined;

/**
 * `disruptFirstExecute` severs the transport on the first Execute request
 * instead of letting `routes` handle it, before any response is sent — this
 * is what a real mid-call disconnect looks like (server-side session
 * teardown, or a raw socket reset), as opposed to an application-level gRPC
 * error the router returns intentionally.
 */
async function startH2Server(
  routes: (router: ConnectRouter) => void,
  opts?: { disruptFirstExecute?: "session" | "socket" },
): Promise<{ baseUrl: string; close(): Promise<void> }> {
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
        req.stream.session?.destroy();
      } else {
        // req.stream.session.socket is a Proxy that throws
        // ERR_HTTP2_NO_SOCKET_MANIPULATION on destroy(); use the raw socket
        // captured via the server's "connection" event instead, and
        // resetAndDestroy() so the client observes an actual ECONNRESET
        // rather than a clean FIN.
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
  if (!addr || typeof addr === "string") throw new Error("failed to bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

let activeServer: Awaited<ReturnType<typeof startH2Server>> | undefined;
let activeClient: ProcessClient | undefined;

afterEach(async () => {
  activeClient?.abort();
  activeClient = undefined;
  await activeServer?.close();
  activeServer = undefined;
});

describe("ProcessClient.run", () => {
  it("runs a command via /bin/sh -c and returns stdout/stderr/exitCode", async () => {
    activeServer = await startH2Server((router) => {
      router.service(ProcessService, {
        execute: (req) => {
          lastReceivedCommand = req.config?.command;
          return create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new TextEncoder().encode("hello world\n"),
            stderr: new Uint8Array(),
          });
        },
      });
    });
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });
    const result = await activeClient.run(
      "echo hello world",
      30_000,
      new AbortController().signal,
    );
    expect(result).toEqual({
      exitCode: 0,
      stdout: "hello world\n",
      stderr: "",
    });
    expect(lastReceivedCommand).toEqual(["/bin/sh", "-c", "echo hello world"]);
  });

  it("returns a non-zero exit code as a normal result, not a thrown error", async () => {
    activeServer = await startH2Server((router) => {
      router.service(ProcessService, {
        execute: () =>
          create(ExecuteResponseSchema, {
            exitCode: 127,
            stdout: new Uint8Array(),
            stderr: new TextEncoder().encode("sh: nope: not found\n"),
          }),
      });
    });
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });
    const result = await activeClient.run(
      "nope",
      30_000,
      new AbortController().signal,
    );
    expect(result.exitCode).toBe(127);
    expect(result.stderr).toBe("sh: nope: not found\n");
  });

  it("maps a response exceeding readMaxBytes to resource_exhausted", async () => {
    activeServer = await startH2Server((router) => {
      router.service(ProcessService, {
        execute: () =>
          create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new Uint8Array(64 * 1024),
            stderr: new Uint8Array(),
          }),
      });
    });
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      // Small enough that the real decoded message size trips the transport's
      // own wire-level limit — this exercises the actual gRPC/h2 stack, not a
      // client-side pre-check.
      maxCommandOutputSize: 16,
    });
    await expect(
      activeClient.run("big", 30_000, new AbortController().signal),
    ).rejects.toMatchObject({
      code: "resource_exhausted",
    } satisfies Partial<SandboxdRpcError>);
  });

  it("lets one cancelled concurrent call not affect the other", async () => {
    let releaseSlow: (() => void) | undefined;
    const slowGate = new Promise<void>((resolve) => {
      releaseSlow = resolve;
    });
    activeServer = await startH2Server((router) => {
      router.service(ProcessService, {
        execute: async (req) => {
          if (req.config?.command?.[2] === "slow") {
            await slowGate;
          }
          return create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new TextEncoder().encode(req.config?.command?.[2] ?? ""),
            stderr: new Uint8Array(),
          });
        },
      });
    });
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });

    const cancelController = new AbortController();
    const slowRun = activeClient.run("slow", 30_000, cancelController.signal);
    const fastRun = activeClient.run(
      "fast",
      30_000,
      new AbortController().signal,
    );

    cancelController.abort(new Error("caller cancelled"));
    // Must reject with the caller's own abort reason, not get reclassified
    // as a transport failure just because Connect reports it as Canceled.
    await expect(slowRun).rejects.toThrow("caller cancelled");
    await expect(fastRun).resolves.toEqual({
      exitCode: 0,
      stdout: "fast",
      stderr: "",
    });
    releaseSlow?.();
  });

  it("classifies a server-side HTTP/2 session teardown as a connection error", async () => {
    activeServer = await startH2Server(
      (router) => {
        router.service(ProcessService, {
          execute: () => create(ExecuteResponseSchema, {}),
        });
      },
      { disruptFirstExecute: "session" },
    );
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });
    // Asserts the actual Connect code that reached classifyError(), not just
    // the outcome — this is what makes the test a genuine regression guard:
    // Code.Canceled must be classified alongside Unavailable, not fall
    // through to SandboxdRpcError.
    await expect(
      activeClient.run("echo hi", 30_000, new AbortController().signal),
    ).rejects.toSatisfy(
      (err: unknown) =>
        err instanceof SandboxConnectionError &&
        err.kind === "socket" &&
        err.cause instanceof ConnectError &&
        err.cause.code === Code.Canceled,
    );
  });

  it("classifies a server-side raw socket reset (ECONNRESET) as a connection error", async () => {
    activeServer = await startH2Server(
      (router) => {
        router.service(ProcessService, {
          execute: () => create(ExecuteResponseSchema, {}),
        });
      },
      { disruptFirstExecute: "socket" },
    );
    activeClient = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });
    await expect(
      activeClient.run("echo hi", 30_000, new AbortController().signal),
    ).rejects.toSatisfy(
      (err: unknown) =>
        err instanceof SandboxConnectionError &&
        err.kind === "socket" &&
        err.cause instanceof ConnectError &&
        err.cause.code === Code.Aborted,
    );
  });

  it("throws SandboxClosedError for any call after abort()", async () => {
    activeServer = await startH2Server((router) => {
      router.service(ProcessService, {
        execute: () =>
          create(ExecuteResponseSchema, {
            exitCode: 0,
            stdout: new Uint8Array(),
            stderr: new Uint8Array(),
          }),
      });
    });
    const client = new ProcessClient({
      grpcBaseUrl: activeServer.baseUrl,
      maxCommandOutputSize: 1024 * 1024,
    });
    client.abort();
    await expect(
      client.run("echo hi", 30_000, new AbortController().signal),
    ).rejects.toBeInstanceOf(SandboxClosedError);
  });
});
