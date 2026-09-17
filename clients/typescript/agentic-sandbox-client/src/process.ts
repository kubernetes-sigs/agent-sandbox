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

import { ERROR_DETAIL_MAX_BYTES } from "./constants.js";
import {
  SandboxClosedError,
  SandboxConnectionError,
  SandboxdRpcError,
  SandboxError,
  SandboxTimeoutError,
} from "./exceptions.js";
import type { ExecutionResult } from "./types.js";

/**
 * gRPC process layer for sandboxd's ProcessService. Owns the optional
 * protobuf-es/Connect-ES dependencies (lazily imported), the HTTP/2 session,
 * and RPC error translation. Not part of the public API — see commands.ts.
 * @internal
 */
export interface ProcessClientOptions {
  /** e.g. "http://127.0.0.1:54322", generation-scoped, never re-resolved. */
  grpcBaseUrl: string;
  maxCommandOutputSize: number;
}

type GrpcDeps = {
  create: typeof import("@bufbuild/protobuf")["create"];
  createClient: typeof import("@connectrpc/connect")["createClient"];
  Code: typeof import("@connectrpc/connect")["Code"];
  ConnectError: typeof import("@connectrpc/connect")["ConnectError"];
  createGrpcTransport: typeof import("@connectrpc/connect-node")["createGrpcTransport"];
  Http2SessionManager: typeof import("@connectrpc/connect-node")["Http2SessionManager"];
  ExecuteRequestSchema: typeof import("./_proto/process/v1/process_pb.js")["ExecuteRequestSchema"];
  ProcessConfigSchema: typeof import("./_proto/process/v1/process_pb.js")["ProcessConfigSchema"];
  ProcessService: typeof import("./_proto/process/v1/process_pb.js")["ProcessService"];
};

let depsPromise: Promise<GrpcDeps> | null = null;

function isModuleNotFoundError(err: unknown): boolean {
  const code = (err as { code?: string } | undefined)?.code;
  if (code === "ERR_MODULE_NOT_FOUND" || code === "MODULE_NOT_FOUND") {
    return true;
  }
  // A loader can wrap the original resolution failure in its own error with
  // the real one attached as `cause` (this is what Node's `--experimental-*`
  // loaders and some test-mocking harnesses do); check one level down too.
  const cause = (err as { cause?: unknown } | undefined)?.cause;
  return cause !== undefined && cause !== err && isModuleNotFoundError(cause);
}

/**
 * Single-flight lazy loader for the optional gRPC dependencies. A missing
 * package (unresolvable import) is reported as a fixed, actionable
 * SandboxError; any other failure (a bug in one of those modules) propagates
 * unchanged so it is never mistaken for "not installed". The failure cache is
 * cleared on either outcome so a later install can be picked up without
 * restarting the process.
 */
async function loadGrpcDeps(): Promise<GrpcDeps> {
  if (!depsPromise) {
    depsPromise = (async () => {
      try {
        const [protobuf, connect, connectNode, processPb] = await Promise.all([
          import("@bufbuild/protobuf"),
          import("@connectrpc/connect"),
          import("@connectrpc/connect-node"),
          import("./_proto/process/v1/process_pb.js"),
        ]);
        return {
          create: protobuf.create,
          createClient: connect.createClient,
          Code: connect.Code,
          ConnectError: connect.ConnectError,
          createGrpcTransport: connectNode.createGrpcTransport,
          Http2SessionManager: connectNode.Http2SessionManager,
          ExecuteRequestSchema: processPb.ExecuteRequestSchema,
          ProcessConfigSchema: processPb.ProcessConfigSchema,
          ProcessService: processPb.ProcessService,
        };
      } catch (err) {
        depsPromise = null;
        if (isModuleNotFoundError(err)) {
          throw new SandboxError(
            "sandbox.commands.run() requires optional dependencies that are not installed. " +
              "Run: npm install @bufbuild/protobuf @connectrpc/connect @connectrpc/connect-node",
            { telemetryCode: "missing_dependency", cause: err },
          );
        }
        throw err;
      }
    })();
  }
  return depsPromise;
}

function truncateUtf8(s: string, maxBytes: number): string {
  const bytes = new TextEncoder().encode(s);
  if (bytes.length <= maxBytes) return s;
  return new TextDecoder("utf-8", { fatal: false }).decode(
    bytes.slice(0, maxBytes),
  );
}

/**
 * Maps a Connect numeric status code to its known snake_case name (e.g.
 * "resource_exhausted") by inverting the live Code enum, so this tracks
 * whatever the installed @connectrpc/connect version defines rather than a
 * hardcoded table. Returns "unknown" for an unrecognized code.
 */
function connectCodeName(code: number, codeEnum: GrpcDeps["Code"]): string {
  for (const [name, value] of Object.entries(codeEnum)) {
    if (
      typeof value === "number" &&
      value === code &&
      Number.isNaN(Number(name))
    ) {
      return name.replace(/([a-z0-9])([A-Z])/g, "$1_$2").toLowerCase();
    }
  }
  return "unknown";
}

export class ProcessClient {
  private closed = false;
  private sessionManager: { abort(reason?: Error): void } | null = null;
  private client: {
    execute: (
      req: unknown,
      opts: { timeoutMs: number; signal: AbortSignal },
    ) => Promise<{ exitCode: number; stdout: Uint8Array; stderr: Uint8Array }>;
  } | null = null;
  private deps: GrpcDeps | null = null;
  private initPromise: Promise<void> | null = null;

  constructor(private readonly opts: ProcessClientOptions) {}

  private async ensureInit(): Promise<void> {
    if (this.closed) {
      throw new SandboxClosedError("ProcessClient is closed");
    }
    if (!this.initPromise) {
      this.initPromise = (async () => {
        const deps = await loadGrpcDeps();
        if (this.closed) return;
        const sessionManager = new deps.Http2SessionManager(
          this.opts.grpcBaseUrl,
        );
        const transport = deps.createGrpcTransport({
          baseUrl: this.opts.grpcBaseUrl,
          sessionManager,
          readMaxBytes: this.opts.maxCommandOutputSize,
        });
        if (this.closed) {
          sessionManager.abort();
          return;
        }
        this.deps = deps;
        this.sessionManager = sessionManager;
        this.client = deps.createClient(
          deps.ProcessService,
          transport,
        ) as unknown as typeof this.client;
      })().catch((err) => {
        // Clear the cache on failure so a later call (e.g. after installing
        // the optional dependencies, or once a transient error clears) gets
        // a fresh attempt instead of a permanently-rejected promise.
        this.initPromise = null;
        throw err;
      });
    }
    await this.initPromise;
    if (this.closed || !this.client || !this.deps) {
      throw new SandboxClosedError("ProcessClient is closed");
    }
  }

  /**
   * Runs `/bin/sh -c <command>` to completion via sandboxd's Execute RPC.
   * A non-zero exit code is a normal ExecutionResult, not a thrown error.
   */
  async run(
    command: string,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<ExecutionResult> {
    await this.ensureInit();
    const deps = this.deps as GrpcDeps;
    const client = this.client as NonNullable<typeof this.client>;

    const req = deps.create(deps.ExecuteRequestSchema, {
      config: deps.create(deps.ProcessConfigSchema, {
        command: ["/bin/sh", "-c", command],
      }),
    });

    try {
      const resp = await client.execute(req, { timeoutMs, signal });
      const decoder = new TextDecoder("utf-8", { fatal: false });
      return {
        exitCode: resp.exitCode,
        stdout: decoder.decode(resp.stdout),
        stderr: decoder.decode(resp.stderr),
      };
    } catch (err) {
      // A caller-driven abort (our own signal firing) must surface as the
      // caller's own reason, never as a connection error — checked before
      // classifyError() so it can safely treat Canceled/Aborted as transport
      // failures below.
      if (signal.aborted) throw signal.reason;
      throw this.classifyError(err, deps);
    }
  }

  private classifyError(err: unknown, deps: GrpcDeps): Error {
    if (err instanceof deps.ConnectError) {
      const detail = truncateUtf8(
        err.rawMessage || err.message,
        ERROR_DETAIL_MAX_BYTES,
      );
      if (err.code === deps.Code.DeadlineExceeded) {
        return new SandboxTimeoutError("sandboxd Execute call timed out", {
          cause: err,
        });
      }
      if (err.code === deps.Code.Unavailable) {
        return new SandboxConnectionError(
          "sandboxd gRPC endpoint is unavailable",
          "unavailable",
          { cause: err, detail },
        );
      }
      // Canceled and Aborted are what @connectrpc/connect-node reports for a
      // session the server tore down mid-call and for ECONNRESET/
      // stream-destroyed errors, respectively (see node-error.js's
      // connectErrorFromNodeReason and http2-session-manager.js). sandboxd's
      // ProcessService never returns either as an application status, so
      // both mean the transport dropped out from under this call, same as a
      // socket-level failure.
      if (err.code === deps.Code.Canceled || err.code === deps.Code.Aborted) {
        return new SandboxConnectionError(
          "sandboxd gRPC session was disconnected mid-call",
          "socket",
          { cause: err, detail },
        );
      }
      return new SandboxdRpcError(
        `sandboxd Execute call failed: ${connectCodeName(err.code, deps.Code)}`,
        connectCodeName(err.code, deps.Code),
        { cause: err, detail },
      );
    }
    return err instanceof Error ? err : new Error(String(err));
  }

  /** Aborts the HTTP/2 session and prevents any further use of this client. */
  abort(): void {
    this.closed = true;
    this.sessionManager?.abort();
    this.sessionManager = null;
    this.client = null;
    this.deps = null;
  }
}
