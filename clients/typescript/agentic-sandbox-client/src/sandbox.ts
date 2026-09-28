// Copyright 2025 The Kubernetes Authors.
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

import type * as k8s from "@kubernetes/client-node";
import { SandboxCommands } from "./commands.js";
import {
  createConnectionStrategy,
  type SandboxdConnectionStrategy,
  type SandboxdTransport,
} from "./connection.js";
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  CLEANUP_TIMEOUT_MS,
  DEFAULT_MAX_COMMAND_OUTPUT_SIZE,
  DEFAULT_MAX_DOWNLOAD_SIZE,
  DEFAULT_MAX_METADATA_RESPONSE_SIZE,
  DEFAULT_MAX_UPLOAD_SIZE,
  DEFAULT_OPERATION_TIMEOUT_MS,
  DEFAULT_PORT_FORWARD_READY_TIMEOUT_MS,
  DEFAULT_SANDBOXD_GRPC_PORT,
  DEFAULT_SANDBOXD_REST_PORT,
} from "./constants.js";
import {
  isK8s404,
  SandboxClosedError,
  SandboxConnectionError,
  SandboxdApiError,
  SandboxError,
  SandboxTimeoutError,
} from "./exceptions.js";
import { SandboxFiles } from "./files.js";
import { noopLogger } from "./logger.js";
import { ProcessClient, type ProcessSpec } from "./process.js";
import {
  resolveSandboxPath,
  SandboxdRestClient,
  SourceFailure,
} from "./rest.js";
import type { Span, TracerManager } from "./trace-manager.js";
import { spanErrorStatusCode, withSpan } from "./trace-manager.js";
import type {
  DeleteOptions,
  DirectoryListing,
  ExecutionResult,
  FileCallOptions,
  Logger,
  ProcessOptions,
  RunOptions,
  RuntimeCallOptions,
  SandboxdConnectivity,
  SandboxdOptions,
  SandboxHealth,
  SandboxMetadata,
  WriteOptions,
} from "./types.js";

/**
 * Races an operation against a timeout and always releases the timeout timer.
 * The timeout callback may return the timeout value or throw a timeout error.
 * @internal — not part of the public API.
 */
export async function raceWithTimeout<T>(
  operation: Promise<T>,
  timeoutMs: number,
  onTimeout: () => T,
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<T>((resolve, reject) => {
        timer = setTimeout(() => {
          try {
            resolve(onTimeout());
          } catch (err) {
            reject(err);
          }
        }, timeoutMs);
      }),
    ]);
  } finally {
    if (timer !== undefined) {
      clearTimeout(timer);
    }
  }
}

/** @internal — fully validated/defaulted form of SandboxdOptions. */
export interface ResolvedSandboxdOptions {
  connectivity: SandboxdConnectivity;
  restPort: number;
  grpcPort: number;
  portForwardReadyTimeoutMs: number;
  maxDownloadSize: number;
  maxUploadSize: number;
  maxMetadataResponseSize: number;
  maxCommandOutputSize: number;
}

function validateBoundedInt(
  name: string,
  value: number | undefined,
  max: number,
): number | undefined {
  if (value === undefined) return undefined;
  if (!Number.isInteger(value) || value <= 0 || value > max) {
    throw new SandboxError(
      `${name} must be a positive integer <= ${max}, got: ${value}`,
      { telemetryCode: "invalid_argument" },
    );
  }
  return value;
}

/**
 * Validates and defaults a SandboxdOptions bag. Every numeric field defaults
 * only when `undefined` — an explicit 0, NaN, Infinity, or negative value is
 * rejected rather than silently treated as "unlimited".
 * @internal
 */
export function normalizeSandboxdOptions(
  opts?: SandboxdOptions,
): ResolvedSandboxdOptions {
  const resolved: ResolvedSandboxdOptions = {
    connectivity: validateConnectivity(opts?.connectivity),
    restPort:
      validateBoundedInt("sandboxd.restPort", opts?.restPort, 65535) ??
      DEFAULT_SANDBOXD_REST_PORT,
    grpcPort:
      validateBoundedInt("sandboxd.grpcPort", opts?.grpcPort, 65535) ??
      DEFAULT_SANDBOXD_GRPC_PORT,
    portForwardReadyTimeoutMs:
      validateBoundedInt(
        "sandboxd.portForwardReadyTimeoutMs",
        opts?.portForwardReadyTimeoutMs,
        2147483647,
      ) ?? DEFAULT_PORT_FORWARD_READY_TIMEOUT_MS,
    maxDownloadSize:
      validateBoundedInt(
        "sandboxd.maxDownloadSize",
        opts?.maxDownloadSize,
        Number.MAX_SAFE_INTEGER,
      ) ?? DEFAULT_MAX_DOWNLOAD_SIZE,
    maxUploadSize:
      validateBoundedInt(
        "sandboxd.maxUploadSize",
        opts?.maxUploadSize,
        Number.MAX_SAFE_INTEGER,
      ) ?? DEFAULT_MAX_UPLOAD_SIZE,
    maxMetadataResponseSize:
      validateBoundedInt(
        "sandboxd.maxMetadataResponseSize",
        opts?.maxMetadataResponseSize,
        Number.MAX_SAFE_INTEGER,
      ) ?? DEFAULT_MAX_METADATA_RESPONSE_SIZE,
    // Must fit the gRPC transport's own receive-limit ceiling (~4 GiB).
    maxCommandOutputSize:
      validateBoundedInt(
        "sandboxd.maxCommandOutputSize",
        opts?.maxCommandOutputSize,
        0xffffffff,
      ) ?? DEFAULT_MAX_COMMAND_OUTPUT_SIZE,
  };
  // With in-cluster connectivity both would dial the same pod address, so
  // REST and gRPC must be on distinct ports; the Go client rejects this too.
  if (resolved.restPort === resolved.grpcPort) {
    throw new SandboxError(
      `sandboxd.restPort and sandboxd.grpcPort must differ (both ${resolved.restPort})`,
      { telemetryCode: "invalid_argument" },
    );
  }
  return resolved;
}

const CONNECTIVITY_VALUES: readonly SandboxdConnectivity[] = [
  "port-forward",
  "in-cluster-service",
  "in-cluster-pod-ip",
];

function validateConnectivity(
  value: SandboxdConnectivity | undefined,
): SandboxdConnectivity {
  if (value === undefined) return "port-forward";
  if (!CONNECTIVITY_VALUES.includes(value)) {
    throw new SandboxError(
      `sandboxd.connectivity must be one of ${CONNECTIVITY_VALUES.map((v) => `"${v}"`).join(", ")}, got: ${String(value)}`,
      { telemetryCode: "invalid_argument" },
    );
  }
  return value;
}

function validateTimeoutMs(name: string, value: number | undefined): number {
  return (
    validateBoundedInt(name, value, 2147483647) ?? DEFAULT_OPERATION_TIMEOUT_MS
  );
}

/**
 * Validates run()'s argv/env/cwd shape before any connection is made, so a
 * malformed call fails fast with invalid_argument instead of surfacing as a
 * sandboxd RPC error (or, for a non-string, being silently coerced by
 * protobuf-es). Path semantics of `cwd` are left to sandboxd, which confines
 * it to the sandbox root.
 */
function validateProcessSpec(
  command: string,
  args: readonly string[],
  opts: ProcessOptions | undefined,
): ProcessSpec {
  if (typeof command !== "string" || command === "") {
    throw invalidArgumentError("command must be a non-empty string");
  }
  if (!Array.isArray(args) || args.some((a) => typeof a !== "string")) {
    throw invalidArgumentError("args must be an array of strings");
  }
  const env = opts?.env;
  if (env !== undefined) {
    if (typeof env !== "object" || env === null) {
      throw invalidArgumentError("env must be an object of string values");
    }
    for (const [key, value] of Object.entries(env)) {
      if (key === "" || key.includes("=") || typeof value !== "string") {
        throw invalidArgumentError(
          "env keys must be non-empty and contain no '=', and values must be strings",
        );
      }
    }
  }
  const cwd = opts?.cwd;
  if (cwd !== undefined && typeof cwd !== "string") {
    throw invalidArgumentError("cwd must be a string");
  }
  return {
    command: [command, ...args],
    ...(env !== undefined && { env }),
    ...(cwd !== undefined && cwd !== "" && { cwd }),
  };
}

function invalidArgumentError(message: string): SandboxError {
  return new SandboxError(message, { telemetryCode: "invalid_argument" });
}

/**
 * The executable's base name only, so a span never carries a full path
 * (paths are kept out of telemetry, as for the files API).
 */
function executableName(command: string): string {
  return command.slice(command.lastIndexOf("/") + 1);
}

function classifyForTelemetry(
  err: unknown,
  userSignal: AbortSignal | undefined,
): { code: string; message: string } {
  // A user-cancelled operation can reject with the caller's own reason
  // (any shape, not necessarily a SandboxError) — classify it before
  // inspecting the error's own type.
  if (userSignal?.aborted && err === userSignal.reason) {
    return { code: "cancelled", message: "sandbox operation failed" };
  }
  if (err instanceof SandboxError) {
    return { code: err.telemetryCode, message: "sandbox operation failed" };
  }
  return { code: "unknown", message: "sandbox operation failed" };
}

/**
 * One lazily-established connection to sandboxd inside the Pod: the
 * transport (a port-forward tunnel, or a pod-network address) plus the REST
 * and gRPC clients scoped to it. REST and
 * gRPC always come from the same generation; a transport failure discards
 * the whole thing rather than one leg of it. Never exposed as a public
 * field/getter type — only used inside this module's private methods, and
 * threaded into files.ts/commands.ts facades through closures over plain
 * public types, so it never appears in Sandbox's declaration surface.
 */
interface ConnectionGeneration {
  id: number;
  transport: SandboxdTransport;
  rest: SandboxdRestClient;
  process: ProcessClient;
  /** Aborted to invalidate every in-flight operation sharing this generation. */
  abortController: AbortController;
}

/**
 * Internal initialisation bag passed from SandboxClient to Sandbox constructor.
 * Not part of the public API.
 */
export interface SandboxInit {
  claimName: string;
  sandboxName: string;
  podName: string;
  /** Pod IP chosen from status.podIPs; "" when unknown. */
  podIP?: string;
  /** status.serviceFQDN; "" when the Sandbox has no headless Service. */
  serviceFQDN?: string;
  namespace: string;
  customObjectsApi: k8s.CustomObjectsApi;
  kubeConfig: k8s.KubeConfig;
  sandboxdOptions: ResolvedSandboxdOptions;
  tracingManager: TracerManager | null;
  traceServiceName: string;
  logger?: Logger;
}

/**
 * A claimed Sandbox resource handle: stable identity (claim / sandbox / pod
 * names + namespace) plus lifecycle (`close()` / `closeLocal()`) and lazy
 * connectivity to the sandbox runtime via `.files` and `.commands`. Obtain
 * instances via SandboxClient.createSandbox() or getSandbox().
 *
 * The connection to sandboxd (REST files API + gRPC process API) is
 * established lazily on first use and is never retried automatically: a
 * failed operation is never replayed, and a transport failure discards the
 * whole connection generation so the next call reconnects from scratch. See
 * the SDK README for the full connectivity/trust model.
 */
export class Sandbox {
  readonly claimName: string;
  readonly sandboxName: string;
  readonly podName: string;
  /**
   * The pod IP (IPv4 preferred) observed when the handle was created, or ""
   * when unknown. Not refreshed if the pod is later rescheduled.
   */
  readonly podIP: string;
  /**
   * In-cluster DNS name of the Sandbox's headless Service, or "" when it has
   * none (spec.service unset or false).
   */
  readonly serviceFQDN: string;
  readonly namespace: string;

  protected readonly tracingManager: TracerManager | null;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;
  protected readonly logger: Logger;

  private readonly connectionStrategy: SandboxdConnectionStrategy;
  private readonly sandboxdOptions: ResolvedSandboxdOptions;
  private readonly traceServiceName: string;

  private _isClosed = false;
  private _inflightCount = 0;
  private _drainResolvers: Array<() => void> = [];

  private readonly connectAbortController = new AbortController();
  private readonly lifecycleAbortController = new AbortController();
  private generationCounter = 0;
  private currentGeneration: ConnectionGeneration | null = null;
  private connectingPromise: Promise<ConnectionGeneration> | null = null;
  private invalidationPromise: Promise<void> | null = null;
  private releaseLocalPromise: Promise<void> | null = null;
  private deletionPromise: Promise<void> | null = null;

  private _files?: SandboxFiles;
  private _commands?: SandboxCommands;

  constructor(init: SandboxInit) {
    this.claimName = init.claimName;
    this.sandboxName = init.sandboxName;
    this.podName = init.podName;
    this.podIP = init.podIP ?? "";
    this.serviceFQDN = init.serviceFQDN ?? "";
    this.namespace = init.namespace;
    this.customObjectsApi = init.customObjectsApi;
    this.sandboxdOptions = init.sandboxdOptions;
    this.tracingManager = init.tracingManager;
    this.traceServiceName = init.traceServiceName;
    this.logger = init.logger ?? noopLogger;
    this.connectionStrategy = createConnectionStrategy(
      init.sandboxdOptions.connectivity,
      {
        kubeConfig: init.kubeConfig,
        namespace: this.namespace,
        podName: this.podName,
        podIP: this.podIP,
        serviceFQDN: this.serviceFQDN,
        restPort: init.sandboxdOptions.restPort,
        grpcPort: init.sandboxdOptions.grpcPort,
        handshakeTimeoutMs: init.sandboxdOptions.portForwardReadyTimeoutMs,
        logger: this.logger,
      },
    );
  }

  /**
   * Returns true if the handle has not been closed.
   */
  get isActive(): boolean {
    return !this._isClosed;
  }

  /** Runs commands inside the sandbox. Connects to sandboxd lazily. */
  get commands(): SandboxCommands {
    if (!this._commands) {
      this._commands = new SandboxCommands({
        run: (command, args, opts) => this.runCommandImpl(command, args, opts),
      });
    }
    return this._commands;
  }

  /** Reads/writes files inside the sandbox. Connects to sandboxd lazily. */
  get files(): SandboxFiles {
    if (!this._files) {
      this._files = new SandboxFiles({
        read: (path, opts) => this.readFileImpl(path, opts),
        write: (path, content, opts) => this.writeFileImpl(path, content, opts),
        readStream: (path, opts) => this.readStreamImpl(path, opts),
        writeStream: (path, content, opts) =>
          this.writeStreamImpl(path, content, opts),
        exists: (path, opts) => this.existsImpl(path, opts),
        list: (path, opts) => this.listImpl(path, opts),
        delete: (path, opts) => this.deleteFileImpl(path, opts),
      });
    }
    return this._files;
  }

  /**
   * Probes sandboxd's `/v1/health`. Resolves with sandboxd's report when it
   * is ready. Connects to sandboxd lazily, and that connect already waits
   * for sandboxd to become healthy: a first call against a not-yet-ready
   * sandboxd therefore fails with the connect's timeout or connection error,
   * not a 503. Only on an already-established connection does an unready
   * sandboxd (e.g. shutting down) reject with a `SandboxdApiError` (status
   * 503). The SDK never retries automatically.
   */
  health(opts?: RuntimeCallOptions): Promise<SandboxHealth> {
    return this.healthImpl(opts);
  }

  /**
   * Reads sandboxd's `/v1/metadata`: the non-sensitive, workload-scoped
   * environment the orchestrator injected. sandboxd serves only variables
   * matching its `--metadata-env-prefix` (default `SANDBOX_`) and withholds
   * credential-looking names, so this is never sandboxd's full environment.
   * Connects to sandboxd lazily. The SDK never retries automatically.
   */
  metadata(opts?: RuntimeCallOptions): Promise<SandboxMetadata> {
    return this.metadataImpl(opts);
  }

  /**
   * Marks the handle closed and ends its tracing lifecycle span (if any).
   * Does NOT delete the SandboxClaim from Kubernetes.
   * Use this to release local resources without destroying the live claim —
   * e.g. SandboxClient.getSandbox() evicting a stale cached handle whose claim
   * may no longer be owned by it.
   */
  async closeLocal(): Promise<void> {
    this._isClosed = true;
    this.connectAbortController.abort(
      new SandboxClosedError("Sandbox handle is closing"),
    );
    await this.releaseLocal();
  }

  /**
   * Closes the handle and deletes the SandboxClaim.
   *
   * A missing claim (404) is treated as success. Any other failure — including
   * the cleanup timeout — is re-thrown as a {@link SandboxError} so callers can
   * retry; the handle is still marked closed and the claim may be re-deleted
   * via {@link SandboxClient.deleteSandbox}.
   */
  async close(): Promise<void> {
    // Prevent new work immediately so the in-flight count stabilises.
    this._isClosed = true;
    this.connectAbortController.abort(
      new SandboxClosedError("Sandbox handle is closing"),
    );

    // Drain connected in-flight work; give up after CLEANUP_TIMEOUT_MS, or as
    // soon as a concurrent closeLocal() fires the lifecycle abort, so close()
    // is always bounded.
    await raceWithTimeout(
      Promise.race([
        this.drainInflight(),
        this.rejectOnAbort(this.lifecycleAbortController.signal).catch(
          () => undefined,
        ),
      ]),
      CLEANUP_TIMEOUT_MS,
      () => undefined,
    );

    await this.releaseLocal();

    if (this.claimName) {
      if (!this.deletionPromise) {
        this.deletionPromise = this.deleteClaim();
      }
      await this.deletionPromise;
    }
  }

  private async deleteClaim(): Promise<void> {
    this.logger.info(`Deleting SandboxClaim: ${this.claimName}`);
    try {
      await raceWithTimeout(
        this.customObjectsApi.deleteNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: this.namespace,
          plural: CLAIM_PLURAL_NAME,
          name: this.claimName,
        }),
        CLEANUP_TIMEOUT_MS,
        () => {
          throw new Error(
            `SandboxClaim cleanup timed out after ${CLEANUP_TIMEOUT_MS}ms`,
          );
        },
      );
    } catch (err: unknown) {
      // Allow a later close() call to retry deletion only.
      this.deletionPromise = null;
      if (isK8s404(err)) {
        return;
      }
      this.logger.error(`Error deleting sandbox claim: ${err}`);
      throw new SandboxError(
        `Failed to delete SandboxClaim '${this.claimName}' in namespace '${this.namespace}'.`,
        { cause: err },
      );
    }
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  /**
   * Resolves once no work is in flight. Only work that has passed the shared
   * connect stage (see runOperation()) counts as in-flight — see
   * runOperation()'s "connected in-flight" accounting.
   */
  private drainInflight(): Promise<void> {
    if (this._inflightCount === 0) return Promise.resolve();
    return new Promise<void>((resolve) => {
      this._drainResolvers.push(resolve);
    });
  }

  private releaseLocal(): Promise<void> {
    if (!this.releaseLocalPromise) {
      this.releaseLocalPromise = (async () => {
        this.lifecycleAbortController.abort(
          new SandboxClosedError("Sandbox handle is closed"),
        );

        const gen = this.currentGeneration;
        this.currentGeneration = null;
        const pendingConnect = this.connectingPromise;
        const pendingInvalidation = this.invalidationPromise;

        await Promise.allSettled([
          gen ? this.teardownGeneration(gen) : Promise.resolve(),
          pendingConnect
            ? pendingConnect
                .then((g) => this.teardownGeneration(g))
                .catch(() => {})
            : Promise.resolve(),
          pendingInvalidation ?? Promise.resolve(),
        ]);

        if (this.tracingManager) {
          try {
            this.tracingManager.endLifecycleSpan();
          } catch (err) {
            this.logger.error(`Failed to end tracing span: ${err}`);
          }
        }
      })();
    }
    return this.releaseLocalPromise;
  }

  private async teardownGeneration(gen: ConnectionGeneration): Promise<void> {
    gen.abortController.abort(
      new SandboxClosedError("Sandbox handle is closed"),
    );
    gen.process.abort();
    await gen.transport.close().catch(() => {});
  }

  private rejectOnAbort(signal: AbortSignal): Promise<never> {
    return new Promise((_resolve, reject) => {
      if (signal.aborted) {
        reject(signal.reason);
        return;
      }
      signal.addEventListener("abort", () => reject(signal.reason), {
        once: true,
      });
    });
  }

  private invalidateGeneration(
    gen: ConnectionGeneration,
    reason: unknown,
  ): void {
    if (this.currentGeneration !== gen) return;
    this.currentGeneration = null;
    gen.abortController.abort(reason);
    const promise = this.teardownGeneration(gen).catch(() => {});
    this.invalidationPromise = promise;
    promise.finally(() => {
      if (this.invalidationPromise === promise) this.invalidationPromise = null;
    });
  }

  /**
   * Returns the current connection generation, establishing one if none
   * exists. Concurrent callers share a single in-flight connect (single
   * flight): a caller's own timeout/abort only stops IT from waiting — the
   * shared connect keeps running (bounded only by connectAbortController and
   * its own deadline) so other waiters, or a later call, can still use it.
   */
  private async ensureConnected(
    callerSignal: AbortSignal,
  ): Promise<ConnectionGeneration> {
    if (!this.isActive) {
      throw new SandboxClosedError("Sandbox handle is closed");
    }

    if (this.invalidationPromise) {
      await Promise.race([
        this.invalidationPromise,
        this.rejectOnAbort(callerSignal),
      ]).catch((err) => {
        if (callerSignal.aborted) throw err;
      });
    }

    if (this.currentGeneration) {
      return this.currentGeneration;
    }
    if (!this.isActive) {
      throw new SandboxClosedError("Sandbox handle is closed");
    }

    if (!this.connectingPromise) {
      const attempt = this.doConnect()
        .then(async (gen) => {
          // connectAbortController fires synchronously at the start of
          // close()/closeLocal(); releaseLocal() may have already nulled
          // currentGeneration and torn down by the time this resolves. Never
          // resurrect a generation for a handle that is closing.
          if (this.connectAbortController.signal.aborted) {
            await this.teardownGeneration(gen);
            throw new SandboxClosedError("Sandbox handle is closed");
          }
          this.currentGeneration = gen;
          return gen;
        })
        .finally(() => {
          this.connectingPromise = null;
        });
      attempt.catch(() => {});
      this.connectingPromise = attempt;
    }
    const shared = this.connectingPromise;
    return Promise.race([shared, this.rejectOnAbort(callerSignal)]);
  }

  private async doConnect(): Promise<ConnectionGeneration> {
    const id = ++this.generationCounter;
    const deadlineMs = this.sandboxdOptions.portForwardReadyTimeoutMs;
    const deadlineController = new AbortController();
    const timer = setTimeout(() => {
      deadlineController.abort(
        new SandboxTimeoutError(
          `sandboxd connection did not become ready within ${deadlineMs}ms`,
        ),
      );
    }, deadlineMs);
    const signal = AbortSignal.any([
      this.connectAbortController.signal,
      deadlineController.signal,
    ]);

    let transport: SandboxdTransport | undefined;
    try {
      transport = await this.connectionStrategy.open();
      this.logger.debug(
        `sandboxd transport opened (connectivity: ${this.connectionStrategy.connectivity})`,
      );

      const rest = new SandboxdRestClient({
        baseUrl: transport.restBaseUrl,
        maxDownloadSize: this.sandboxdOptions.maxDownloadSize,
        maxUploadSize: this.sandboxdOptions.maxUploadSize,
        maxMetadataResponseSize: this.sandboxdOptions.maxMetadataResponseSize,
      });
      await this.waitForHealthy(rest, transport, signal);

      const process = new ProcessClient({
        grpcBaseUrl: transport.grpcBaseUrl,
        maxCommandOutputSize: this.sandboxdOptions.maxCommandOutputSize,
      });

      return {
        id,
        transport,
        rest,
        process,
        abortController: new AbortController(),
      };
    } catch (err) {
      await transport?.close().catch(() => {});
      throw err;
    } finally {
      clearTimeout(timer);
    }
  }

  private async waitForHealthy(
    rest: SandboxdRestClient,
    transport: SandboxdTransport,
    signal: AbortSignal,
  ): Promise<void> {
    while (true) {
      if (signal.aborted) throw signal.reason;
      try {
        await rest.health(signal);
        return;
      } catch (err) {
        const retryable =
          (err instanceof SandboxdApiError && err.status === 503) ||
          err instanceof SandboxConnectionError;
        if (!retryable) throw err;
        const terminal = transport.terminalError();
        if (terminal) throw terminal;
        await this.sleep(200, signal);
      }
    }
  }

  private sleep(ms: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      if (signal.aborted) {
        reject(signal.reason);
        return;
      }
      const timer = setTimeout(resolve, ms);
      signal.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          reject(signal.reason);
        },
        { once: true },
      );
    });
  }

  /**
   * Runs one operation's full lifecycle: deadline setup, shared-connect wait,
   * connected in-flight accounting, and failure classification. Only work
   * inside `fn` (after the connect wait) counts toward drainInflight()/close().
   */
  private async runOperation<T>(
    timeoutMs: number,
    userSignal: AbortSignal | undefined,
    fn: (gen: ConnectionGeneration, signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    if (!this.isActive) {
      throw new SandboxClosedError("Sandbox handle is closed");
    }
    if (userSignal?.aborted) {
      throw userSignal.reason;
    }

    const timeoutController = new AbortController();
    const timer = setTimeout(() => {
      timeoutController.abort(
        new SandboxTimeoutError(`operation timed out after ${timeoutMs}ms`),
      );
    }, timeoutMs);

    const signals: AbortSignal[] = [
      timeoutController.signal,
      this.lifecycleAbortController.signal,
    ];
    if (userSignal) signals.push(userSignal);
    const totalSignal = AbortSignal.any(signals);

    try {
      const gen = await this.ensureConnected(totalSignal);
      // No await between here and the in-flight increment: close()'s drain
      // must not observe a gap where this operation is neither "waiting to
      // connect" nor "counted in-flight".
      if (!this.isActive || this.currentGeneration !== gen) {
        throw new SandboxClosedError("Sandbox handle is closed");
      }
      this._inflightCount++;
      try {
        const requestSignal = AbortSignal.any([
          totalSignal,
          gen.abortController.signal,
        ]);
        try {
          return await fn(gen, requestSignal);
        } catch (err) {
          throw this.classifyOperationFailure(
            err,
            gen,
            timeoutController,
            userSignal,
          );
        }
      } finally {
        this._inflightCount--;
        if (this._inflightCount === 0) {
          const resolvers = this._drainResolvers;
          this._drainResolvers = [];
          for (const resolve of resolvers) resolve();
        }
      }
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * Classifies by the composite signal's own first reason before ever
   * inspecting the raw error shape, so an ambiguous "aborted" error from the
   * transport is never mistaken for a connection failure once the real cause
   * is our own timeout/close/user-cancel. Only once none of those apply does
   * an actual SandboxConnectionError invalidate the generation.
   */
  private classifyOperationFailure(
    err: unknown,
    gen: ConnectionGeneration,
    timeoutController: AbortController,
    userSignal: AbortSignal | undefined,
  ): unknown {
    if (timeoutController.signal.aborted) {
      return timeoutController.signal.reason;
    }
    if (this.lifecycleAbortController.signal.aborted) {
      return new SandboxClosedError("Sandbox handle is closed");
    }
    if (userSignal?.aborted) {
      return userSignal.reason;
    }
    if (gen.abortController.signal.aborted) {
      return new SandboxConnectionError(
        "sandboxd connection was invalidated by a concurrent failure",
        "protocol",
        { cause: err },
      );
    }
    // A writeStream() source failure is never a sandboxd connection problem
    // — keep it out of generation invalidation even if its wrapped value
    // happens to be a SandboxConnectionError, and let it pass through
    // untouched for writeStreamImpl() to unwrap.
    if (err instanceof SourceFailure) {
      return err;
    }
    if (err instanceof SandboxConnectionError) {
      this.invalidateGeneration(gen, err);
    }
    return err;
  }

  /**
   * Wraps runOperation() in a tracing span: only a fixed `sandbox.error.code`
   * and safe status message are recorded on failure — never the raw
   * exception, a path, or a command string.
   */
  private async operate<T>(
    spanSuffix: string,
    timeoutMs: number,
    userSignal: AbortSignal | undefined,
    setAttrs: (span: Span) => void,
    onSuccess: ((span: Span, result: T) => void) | undefined,
    fn: (gen: ConnectionGeneration, signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    return withSpan(
      this.tracingManager?.tracer ?? null,
      this.traceServiceName,
      spanSuffix,
      async (span) => {
        setAttrs(span);
        const result = await this.runOperation(timeoutMs, userSignal, fn);
        onSuccess?.(span, result);
        return result;
      },
      this.tracingManager?.parentContext,
      (span, err) => {
        const { code, message } = classifyForTelemetry(err, userSignal);
        if (span.isRecording()) {
          span.setAttribute("sandbox.error.code", code);
        }
        span.setStatus({ code: spanErrorStatusCode(), message });
      },
    );
  }

  private async runCommandImpl(
    command: string,
    args: readonly string[],
    opts?: RunOptions,
  ): Promise<ExecutionResult> {
    const spec = validateProcessSpec(command, args, opts);
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    const startedAt = Date.now();
    return this.operate<ExecutionResult>(
      "command.run",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording()) {
          span.setAttribute(
            "sandbox.command.executable",
            executableName(command),
          );
        }
      },
      (span, result) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.exit_code", result.exitCode);
        }
      },
      (gen, signal) => {
        // `fn` runs only after the shared connect has resolved, so the
        // remaining budget (not the original total) is what the gRPC
        // deadline should reflect — the connect wait may have consumed a
        // large fraction of it on a cold connection.
        const remainingMs = Math.max(1, timeoutMs - (Date.now() - startedAt));
        return gen.process.run(spec, remainingMs, signal);
      },
    );
  }

  private async healthImpl(opts?: RuntimeCallOptions): Promise<SandboxHealth> {
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<SandboxHealth>(
      "runtime.health",
      timeoutMs,
      opts?.signal,
      () => {},
      undefined,
      (gen, signal) => gen.rest.health(signal),
    );
  }

  private async metadataImpl(
    opts?: RuntimeCallOptions,
  ): Promise<SandboxMetadata> {
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<SandboxMetadata>(
      "runtime.metadata",
      timeoutMs,
      opts?.signal,
      () => {},
      (span, result) => {
        // Only the count: names and values are server-controlled data and
        // stay out of telemetry.
        if (span.isRecording())
          span.setAttribute(
            "sandbox.metadata.env_count",
            Object.keys(result.env).length,
          );
      },
      (gen, signal) => gen.rest.metadata(signal),
    );
  }

  private async readFileImpl(
    path: string,
    opts?: FileCallOptions,
  ): Promise<Uint8Array> {
    resolveSandboxPath(path, "read");
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<Uint8Array>(
      "files.read",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.operation", "read");
      },
      (span, result) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.size", result.byteLength);
      },
      (gen, signal) => gen.rest.read(path, signal),
    );
  }

  private async writeFileImpl(
    path: string,
    content: string | Uint8Array,
    opts?: WriteOptions,
  ): Promise<void> {
    resolveSandboxPath(path, "write");
    if (opts?.mode !== undefined && !/^0[0-7]{3}$/.test(opts.mode)) {
      throw new SandboxError(
        `invalid mode '${opts.mode}': must match ^0[0-7]{3}$`,
        { telemetryCode: "invalid_argument" },
      );
    }
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    const bytes =
      typeof content === "string" ? new TextEncoder().encode(content) : content;
    if (bytes.byteLength > this.sandboxdOptions.maxUploadSize) {
      throw new SandboxError(
        `content of ${bytes.byteLength} bytes exceeds the configured upload limit of ${this.sandboxdOptions.maxUploadSize} bytes`,
        { telemetryCode: "invalid_argument" },
      );
    }
    return this.operate<void>(
      "files.write",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.operation", "write");
          span.setAttribute("sandbox.file.size", bytes.byteLength);
        }
      },
      undefined,
      (gen, signal) =>
        gen.rest.write(path, bytes, { mode: opts?.mode }, signal),
    );
  }

  /**
   * Drives one readStream() call: unlike runOperation()/operate(), the
   * public promise (readStreamImpl's return value) resolves once headers are
   * validated — well before the operation itself is done. This method keeps
   * the operation (in-flight accounting, timeout, generation classification,
   * tracing span) alive until the returned stream reaches a terminal state,
   * by calling `onReady`/`onReadyFailed` as soon as that's known and
   * resolving/rejecting its own returned promise only once the stream
   * finishes — see readStreamImpl()'s use of withSpan() for how the two are
   * stitched together.
   */
  private async runReadStreamOperation(
    timeoutMs: number,
    userSignal: AbortSignal | undefined,
    path: string,
    onReady: (stream: ReadableStream<Uint8Array>) => void,
    onReadyFailed: (err: unknown) => void,
  ): Promise<
    { outcome: "completed"; totalBytes: number } | { outcome: "cancelled" }
  > {
    if (!this.isActive) {
      const err = new SandboxClosedError("Sandbox handle is closed");
      onReadyFailed(err);
      throw err;
    }
    if (userSignal?.aborted) {
      onReadyFailed(userSignal.reason);
      throw userSignal.reason;
    }

    const timeoutController = new AbortController();
    const timer = setTimeout(() => {
      timeoutController.abort(
        new SandboxTimeoutError(`operation timed out after ${timeoutMs}ms`),
      );
    }, timeoutMs);
    const signals: AbortSignal[] = [
      timeoutController.signal,
      this.lifecycleAbortController.signal,
    ];
    if (userSignal) signals.push(userSignal);
    const totalSignal = AbortSignal.any(signals);

    let gen: ConnectionGeneration;
    try {
      gen = await this.ensureConnected(totalSignal);
    } catch (err) {
      clearTimeout(timer);
      onReadyFailed(err);
      throw err;
    }
    // Mirrors runOperation()'s re-check: no await between here and the
    // in-flight increment below.
    if (!this.isActive || this.currentGeneration !== gen) {
      clearTimeout(timer);
      const err = new SandboxClosedError("Sandbox handle is closed");
      onReadyFailed(err);
      throw err;
    }

    this._inflightCount++;
    const requestSignal = AbortSignal.any([
      totalSignal,
      gen.abortController.signal,
    ]);
    const classify = (err: unknown): unknown =>
      this.classifyOperationFailure(err, gen, timeoutController, userSignal);

    let finished = false;
    const finishDrain = (): void => {
      if (finished) return;
      finished = true;
      this._inflightCount--;
      if (this._inflightCount === 0) {
        const resolvers = this._drainResolvers;
        this._drainResolvers = [];
        for (const resolve of resolvers) resolve();
      }
      clearTimeout(timer);
    };

    let restStream: ReadableStream<Uint8Array>;
    try {
      restStream = await gen.rest.readStream(path, requestSignal);
    } catch (err) {
      const classified = classify(err);
      finishDrain();
      onReadyFailed(classified);
      throw classified;
    }

    // The REST-layer stream (wrapDownloadStream() in rest.ts) already errors
    // itself the instant `requestSignal` aborts — via its own listener, and
    // via `reader.closed` rejecting even while its single-slot queue is full
    // and no read() is pending — so re-reading from it here via
    // `restReader.read()`/`restReader.closed` is enough to observe every
    // termination without this method needing its own abort listener.
    return new Promise<
      { outcome: "completed"; totalBytes: number } | { outcome: "cancelled" }
    >((resolveCompletion, rejectCompletion) => {
      let terminal = false;
      const restReader = restStream.getReader();
      let totalBytes = 0;

      const terminate = (
        result:
          | { outcome: "completed"; totalBytes: number }
          | { outcome: "cancelled" }
          | { outcome: "failed"; error: unknown },
      ): void => {
        if (terminal) return;
        terminal = true;
        finishDrain();
        if (result.outcome === "failed") {
          rejectCompletion(result.error);
        } else {
          resolveCompletion(result);
        }
      };

      const publicStream = new ReadableStream<Uint8Array>(
        {
          start: (controller) => {
            restReader.closed.catch((err: unknown) => {
              if (terminal) return;
              const classified = classify(err);
              terminate({ outcome: "failed", error: classified });
              controller.error(classified);
            });
          },
          pull: async (controller) => {
            if (terminal) return;
            let result: ReadableStreamReadResult<Uint8Array>;
            try {
              result = await restReader.read();
            } catch (err) {
              if (terminal) return;
              const classified = classify(err);
              terminate({ outcome: "failed", error: classified });
              controller.error(classified);
              return;
            }
            if (terminal) return;
            if (result.done) {
              const finalBytes = totalBytes;
              terminate({ outcome: "completed", totalBytes: finalBytes });
              controller.close();
              return;
            }
            totalBytes += result.value.byteLength;
            controller.enqueue(result.value);
          },
          cancel: (reason) => {
            // A consumer-initiated cancel is a normal termination, never an
            // error — see the state-machine invariant in wrapDownloadStream().
            terminate({ outcome: "cancelled" });
            restReader.cancel(reason).catch(() => {});
          },
        },
        { highWaterMark: 1 },
      );

      onReady(publicStream);
    });
  }

  private async readStreamImpl(
    path: string,
    opts?: FileCallOptions,
  ): Promise<ReadableStream<Uint8Array>> {
    resolveSandboxPath(path, "read");
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    const userSignal = opts?.signal;

    let readyResolve!: (stream: ReadableStream<Uint8Array>) => void;
    let readyReject!: (err: unknown) => void;
    const ready = new Promise<ReadableStream<Uint8Array>>((resolve, reject) => {
      readyResolve = resolve;
      readyReject = reject;
    });

    const tracingPromise = withSpan(
      this.tracingManager?.tracer ?? null,
      this.traceServiceName,
      "files.read_stream",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.operation", "read_stream");
        }
        const result = await this.runReadStreamOperation(
          timeoutMs,
          userSignal,
          path,
          readyResolve,
          readyReject,
        );
        if (result.outcome === "completed" && span.isRecording()) {
          span.setAttribute("sandbox.file.size", result.totalBytes);
        }
      },
      this.tracingManager?.parentContext,
      (span, err) => {
        const { code, message } = classifyForTelemetry(err, userSignal);
        if (span.isRecording()) {
          span.setAttribute("sandbox.error.code", code);
        }
        span.setStatus({ code: spanErrorStatusCode(), message });
      },
    );
    // Failures already reach the caller via `ready` (pre-header failures) or
    // the returned stream's error (post-ready failures) — this promise's
    // rejection is purely for span lifetime/telemetry and must not also
    // surface as an unhandled rejection.
    tracingPromise.catch(() => {});

    return ready;
  }

  private async writeStreamImpl(
    path: string,
    content: ReadableStream<Uint8Array>,
    opts?: WriteOptions,
  ): Promise<void> {
    resolveSandboxPath(path, "write");
    if (opts?.mode !== undefined && !/^0[0-7]{3}$/.test(opts.mode)) {
      throw new SandboxError(
        `invalid mode '${opts.mode}': must match ^0[0-7]{3}$`,
        { telemetryCode: "invalid_argument" },
      );
    }
    if (content.locked) {
      throw new SandboxError(
        "writeStream content is already locked by another reader",
        { telemetryCode: "invalid_argument" },
      );
    }
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    try {
      await this.operate<number>(
        "files.write_stream",
        timeoutMs,
        opts?.signal,
        (span) => {
          if (span.isRecording()) {
            span.setAttribute("sandbox.file.operation", "write_stream");
          }
        },
        (span, bytesSent) => {
          if (span.isRecording()) {
            span.setAttribute("sandbox.file.size", bytesSent);
          }
        },
        (gen, signal) =>
          gen.rest.writeStream(path, content, { mode: opts?.mode }, signal),
      );
    } catch (err) {
      if (err instanceof SourceFailure) {
        throw err.value;
      }
      throw err;
    }
  }

  private async existsImpl(
    path: string,
    opts?: FileCallOptions,
  ): Promise<boolean> {
    resolveSandboxPath(path, "exists");
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<boolean>(
      "files.exists",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.operation", "exists");
      },
      (span, result) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.exists", result);
      },
      (gen, signal) => gen.rest.exists(path, signal),
    );
  }

  private async listImpl(
    path: string,
    opts?: FileCallOptions,
  ): Promise<DirectoryListing> {
    resolveSandboxPath(path, "list");
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<DirectoryListing>(
      "files.list",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.operation", "list");
      },
      (span, result) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.count", result.entries.length);
      },
      (gen, signal) => gen.rest.list(path, signal),
    );
  }

  private async deleteFileImpl(
    path: string,
    opts?: DeleteOptions,
  ): Promise<void> {
    resolveSandboxPath(path, "delete");
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    return this.operate<void>(
      "files.delete",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording())
          span.setAttribute("sandbox.file.operation", "delete");
      },
      undefined,
      (gen, signal) =>
        gen.rest.delete(path, { recursive: opts?.recursive }, signal),
    );
  }
}
