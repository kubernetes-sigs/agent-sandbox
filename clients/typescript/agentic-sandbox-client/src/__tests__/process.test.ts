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
import { create } from "@bufbuild/protobuf";
import type { ConnectRouter } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { afterEach, describe, expect, it } from "vitest";
import {
  ExecuteResponseSchema,
  ProcessService,
} from "../_proto/process/v1/process_pb.js";
import { SandboxClosedError, type SandboxdRpcError } from "../exceptions.js";
import { ProcessClient } from "../process.js";

let lastReceivedCommand: string[] | undefined;

async function startH2Server(
  routes: (router: ConnectRouter) => void,
): Promise<{ baseUrl: string; close(): Promise<void> }> {
  const server = http2.createServer(connectNodeAdapter({ routes }));
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
    await expect(slowRun).rejects.toBeTruthy();
    await expect(fastRun).resolves.toEqual({
      exitCode: 0,
      stdout: "fast",
      stderr: "",
    });
    releaseSlow?.();
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
