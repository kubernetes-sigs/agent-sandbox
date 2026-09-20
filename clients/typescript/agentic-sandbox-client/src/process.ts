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
import { ERROR_DETAIL_MAX_BYTES } from "./constants.js";
import {
  SandboxClosedError,
  SandboxConnectionError,
  SandboxdRpcError,
  SandboxError,
  SandboxTimeoutError,
} from "./exceptions.js";
import type {
  ExecutionResult,
  ProcessEvent,
  ProcessSignal,
  PtySize,
} from "./types.js";

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

/**
 * A validated sandboxd ProcessConfig: `command` is the full argv (executable
 * first) and is never wrapped in a shell.
 * @internal
 */
export interface ProcessSpec {
  command: readonly string[];
  env?: Readonly<Record<string, string>>;
  cwd?: string;
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
  StartRequestSchema: typeof import("./_proto/process/v1/process_pb.js")["StartRequestSchema"];
  PTYSchema: typeof import("./_proto/process/v1/process_pb.js")["PTYSchema"];
  WriteStdinRequestSchema: typeof import("./_proto/process/v1/process_pb.js")["WriteStdinRequestSchema"];
  SendSignalRequestSchema: typeof import("./_proto/process/v1/process_pb.js")["SendSignalRequestSchema"];
  ResizeTTYRequestSchema: typeof import("./_proto/process/v1/process_pb.js")["ResizeTTYRequestSchema"];
  Signal: typeof import("./_proto/process/v1/process_pb.js")["Signal"];
};

type H2SessionManager = InstanceType<GrpcDeps["Http2SessionManager"]>;

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
          StartRequestSchema: processPb.StartRequestSchema,
          PTYSchema: processPb.PTYSchema,
          WriteStdinRequestSchema: processPb.WriteStdinRequestSchema,
          SendSignalRequestSchema: processPb.SendSignalRequestSchema,
          ResizeTTYRequestSchema: processPb.ResizeTTYRequestSchema,
          Signal: processPb.Signal,
        };
      } catch (err) {
        depsPromise = null;
        if (isModuleNotFoundError(err)) {
          throw new SandboxError(
            "sandbox.commands requires optional dependencies that are not installed. " +
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

function protocolError(message: string): SandboxConnectionError {
  return new SandboxConnectionError(message, "protocol");
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

/**
 * connect-node hangs to the deadline when the server closes a stream with
 * NO_ERROR before any response (macOS session teardown). Fail it as Canceled,
 * like the abrupt form on Linux.
 */
function createSessionManager(
  deps: GrpcDeps,
  baseUrl: string,
): H2SessionManager {
  class ClosedStreamSessionManager extends deps.Http2SessionManager {
    override async request(...args: Parameters<H2SessionManager["request"]>) {
      const stream = await super.request(...args);
      let responded = false;
      stream.once("response", () => {
        responded = true;
      });
      stream.once("close", () => {
        if (!responded && stream.rstCode === http2.constants.NGHTTP2_NO_ERROR) {
          stream.emit(
            "error",
            new deps.ConnectError(
              "HTTP/2 stream closed before a response was received",
              deps.Code.Canceled,
            ),
          );
        }
      });
      return stream;
    }
  }
  return new ClosedStreamSessionManager(baseUrl);
}

type UnaryCallOptions = { timeoutMs: number; signal: AbortSignal };

/** The wire shape of one StartResponse, as far as this module reads it. */
type StartResponseLike = {
  event:
    | { case: "init"; value: { processId: number } }
    | { case: "stdout" | "stderr"; value: Uint8Array }
    | { case: "exit"; value: { exitCode: number } }
    | { case: undefined; value?: undefined };
};

/** Structural view of the generated ProcessService client. */
interface ProcessRpcClient {
  execute(
    req: unknown,
    opts: UnaryCallOptions,
  ): Promise<{ exitCode: number; stdout: Uint8Array; stderr: Uint8Array }>;
  start(
    req: unknown,
    opts: { signal: AbortSignal },
  ): AsyncIterable<StartResponseLike>;
  writeStdin(req: unknown, opts: UnaryCallOptions): Promise<unknown>;
  sendSignal(req: unknown, opts: UnaryCallOptions): Promise<unknown>;
  resizeTTY(req: unknown, opts: UnaryCallOptions): Promise<unknown>;
}

/**
 * A running Start stream, positioned after its InitEvent. `events` ends
 * (done) only after an "exit" event was yielded; a stream that ends any
 * other way rejects instead.
 * @internal
 */
export interface StartedProcess {
  pid: number;
  events: AsyncIterator<ProcessEvent>;
}

export class ProcessClient {
  private closed = false;
  private sessionManager: { abort(reason?: Error): void } | null = null;
  private client: ProcessRpcClient | null = null;
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
        const sessionManager = createSessionManager(
          deps,
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
        ) as unknown as ProcessRpcClient;
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
   * Runs `spec` to completion via sandboxd's Execute RPC. A non-zero exit
   * code is a normal ExecutionResult, not a thrown error.
   */
  async run(
    spec: ProcessSpec,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<ExecutionResult> {
    await this.ensureInit();
    const deps = this.deps as GrpcDeps;
    const client = this.client as NonNullable<typeof this.client>;

    const req = deps.create(deps.ExecuteRequestSchema, {
      config: this.buildConfig(deps, spec),
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
      throw this.classifyError(err, deps, "Execute");
    }
  }

  private buildConfig(deps: GrpcDeps, spec: ProcessSpec) {
    return deps.create(deps.ProcessConfigSchema, {
      command: [...spec.command],
      envVars: { ...spec.env },
      cwd: spec.cwd,
    });
  }

  /**
   * Opens a Start stream and waits for its InitEvent. `signal` governs the
   * stream's whole lifetime — aborting it tears the stream down, which makes
   * sandboxd SIGKILL the process group. No gRPC deadline is set: Connect's
   * `timeoutMs` would bound the entire stream, not just startup, so the
   * caller enforces its own startup budget through `signal`.
   */
  async start(
    spec: ProcessSpec,
    pty: PtySize | undefined,
    signal: AbortSignal,
  ): Promise<StartedProcess> {
    await this.ensureInit();
    const deps = this.deps as GrpcDeps;
    const client = this.client as NonNullable<typeof this.client>;

    const req = deps.create(deps.StartRequestSchema, {
      config: this.buildConfig(deps, spec),
      ...(pty && { pty: deps.create(deps.PTYSchema, pty) }),
    });

    const source = client.start(req, { signal })[Symbol.asyncIterator]();
    const fail = (err: unknown): Error => {
      if (signal.aborted) return signal.reason;
      return this.classifyError(err, deps, "Start");
    };

    let first: IteratorResult<StartResponseLike>;
    try {
      first = await source.next();
    } catch (err) {
      throw fail(err);
    }
    if (first.done || first.value.event.case !== "init") {
      await source.return?.().catch(() => {});
      throw protocolError(
        "sandboxd Start stream did not begin with an InitEvent",
      );
    }
    const pid = first.value.event.value.processId;

    let exited = false;
    const events: AsyncIterator<ProcessEvent> = {
      next: async () => {
        while (true) {
          let step: IteratorResult<StartResponseLike>;
          try {
            step = await source.next();
          } catch (err) {
            throw fail(err);
          }
          if (step.done) {
            if (exited) return { done: true, value: undefined };
            throw protocolError(
              "sandboxd Start stream ended without an ExitEvent",
            );
          }
          const event = step.value.event;
          switch (event.case) {
            case "stdout":
            case "stderr":
              if (exited) {
                throw protocolError("sandboxd sent output after ExitEvent");
              }
              return {
                done: false,
                value: { type: event.case, data: event.value },
              };
            case "exit":
              if (exited) throw protocolError("sandboxd sent two ExitEvents");
              exited = true;
              return {
                done: false,
                value: { type: "exit", exitCode: event.value.exitCode },
              };
            default:
              throw protocolError("sandboxd sent an unexpected Start event");
          }
        }
      },
      return: async () => {
        await source.return?.().catch(() => {});
        return { done: true, value: undefined };
      },
    };
    return { pid, events };
  }

  /** Sends bytes to the process's stdin, or closes it when `input` is null. */
  async writeStdin(
    pid: number,
    input: Uint8Array | null,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<void> {
    await this.unary("WriteStdin", signal, (deps, client) =>
      client.writeStdin(
        deps.create(deps.WriteStdinRequestSchema, {
          processId: pid,
          payload:
            input === null
              ? { case: "eof", value: {} }
              : { case: "input", value: input },
        }),
        { timeoutMs, signal },
      ),
    );
  }

  async sendSignal(
    pid: number,
    sig: ProcessSignal,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<void> {
    await this.unary("SendSignal", signal, (deps, client) =>
      client.sendSignal(
        deps.create(deps.SendSignalRequestSchema, {
          processId: pid,
          signal: deps.Signal[sig],
        }),
        { timeoutMs, signal },
      ),
    );
  }

  async resizeTty(
    pid: number,
    size: PtySize,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<void> {
    await this.unary("ResizeTTY", signal, (deps, client) =>
      client.resizeTTY(
        deps.create(deps.ResizeTTYRequestSchema, { processId: pid, ...size }),
        { timeoutMs, signal },
      ),
    );
  }

  private async unary(
    rpc: string,
    signal: AbortSignal,
    call: (deps: GrpcDeps, client: ProcessRpcClient) => Promise<unknown>,
  ): Promise<void> {
    await this.ensureInit();
    const deps = this.deps as GrpcDeps;
    const client = this.client as NonNullable<typeof this.client>;
    try {
      await call(deps, client);
    } catch (err) {
      if (signal.aborted) throw signal.reason;
      throw this.classifyError(err, deps, rpc);
    }
  }

  private classifyError(err: unknown, deps: GrpcDeps, rpc: string): Error {
    if (err instanceof deps.ConnectError) {
      const detail = truncateUtf8(
        err.rawMessage || err.message,
        ERROR_DETAIL_MAX_BYTES,
      );
      if (err.code === deps.Code.DeadlineExceeded) {
        return new SandboxTimeoutError(`sandboxd ${rpc} call timed out`, {
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
      // @connectrpc/connect itself synthesizes Internal "protocol error: ..."
      // (e.g. "missing status") when a response ends without gRPC trailers,
      // which is what a server-streaming call sees when the HTTP/2 stream or
      // session was cut off. sandboxd's own application errors always carry a
      // real status, so this prefix means the transport failed.
      if (
        err.code === deps.Code.Internal &&
        err.rawMessage.startsWith("protocol error:")
      ) {
        return new SandboxConnectionError(
          "sandboxd gRPC response ended without a status",
          "protocol",
          { cause: err, detail },
        );
      }
      return new SandboxdRpcError(
        `sandboxd ${rpc} call failed: ${connectCodeName(err.code, deps.Code)}`,
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
