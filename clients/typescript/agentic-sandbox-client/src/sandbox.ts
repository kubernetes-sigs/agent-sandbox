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
import { ProcessClient } from "./process.js";
import { resolveSandboxPath, SandboxdRestClient } from "./rest.js";
import type { Span, TracerManager } from "./trace-manager.js";
import { spanErrorStatusCode, withSpan } from "./trace-manager.js";
import { PodTunnel } from "./tunnel.js";
import type {
  DeleteOptions,
  DirectoryListing,
  ExecutionResult,
  FileCallOptions,
  Logger,
  RunOptions,
  SandboxdOptions,
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
  return {
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
}

function validateTimeoutMs(name: string, value: number | undefined): number {
  return (
    validateBoundedInt(name, value, 2147483647) ?? DEFAULT_OPERATION_TIMEOUT_MS
  );
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
 * One lazily-established connection to sandboxd inside the Pod: the local
 * port-forward tunnel plus the REST and gRPC clients scoped to it. REST and
 * gRPC always come from the same generation; a transport failure discards
 * the whole thing rather than one leg of it. Never exposed as a public
 * field/getter type — only used inside this module's private methods, and
 * threaded into files.ts/commands.ts facades through closures over plain
 * public types, so it never appears in Sandbox's declaration surface.
 */
interface ConnectionGeneration {
  id: number;
  tunnel: PodTunnel;
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
  readonly namespace: string;

  protected readonly tracingManager: TracerManager | null;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;
  protected readonly logger: Logger;

  private readonly kubeConfig: k8s.KubeConfig;
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
    this.namespace = init.namespace;
    this.customObjectsApi = init.customObjectsApi;
    this.kubeConfig = init.kubeConfig;
    this.sandboxdOptions = init.sandboxdOptions;
    this.tracingManager = init.tracingManager;
    this.traceServiceName = init.traceServiceName;
    this.logger = init.logger ?? noopLogger;
  }

  /**
   * Returns true if the handle has not been closed.
   */
  get isActive(): boolean {
    return !this._isClosed;
  }

  /** Runs shell commands inside the sandbox. Connects to sandboxd lazily. */
  get commands(): SandboxCommands {
    if (!this._commands) {
      this._commands = new SandboxCommands({
        run: (command, opts) => this.runCommandImpl(command, opts),
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
        exists: (path, opts) => this.existsImpl(path, opts),
        list: (path, opts) => this.listImpl(path, opts),
        delete: (path, opts) => this.deleteFileImpl(path, opts),
      });
    }
    return this._files;
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
    await gen.tunnel.close().catch(() => {});
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

    let tunnel: PodTunnel | undefined;
    try {
      tunnel = new PodTunnel({
        kubeConfig: this.kubeConfig,
        namespace: this.namespace,
        podName: this.podName,
        restTargetPort: this.sandboxdOptions.restPort,
        grpcTargetPort: this.sandboxdOptions.grpcPort,
        handshakeTimeoutMs: deadlineMs,
        logger: this.logger,
      });
      const endpoints = await tunnel.start();

      const rest = new SandboxdRestClient({
        baseUrl: endpoints.restBaseUrl,
        maxDownloadSize: this.sandboxdOptions.maxDownloadSize,
        maxUploadSize: this.sandboxdOptions.maxUploadSize,
        maxMetadataResponseSize: this.sandboxdOptions.maxMetadataResponseSize,
      });
      await this.waitForHealthy(rest, tunnel, signal);

      const process = new ProcessClient({
        grpcBaseUrl: endpoints.grpcBaseUrl,
        maxCommandOutputSize: this.sandboxdOptions.maxCommandOutputSize,
      });

      return {
        id,
        tunnel,
        rest,
        process,
        abortController: new AbortController(),
      };
    } catch (err) {
      await tunnel?.close().catch(() => {});
      throw err;
    } finally {
      clearTimeout(timer);
    }
  }

  private async waitForHealthy(
    rest: SandboxdRestClient,
    tunnel: PodTunnel,
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
        // A connection-level failure could just mean the port-forward
        // hasn't finished establishing yet — but if the apiserver has
        // already rejected the WS upgrade outright (auth denied, Pod not
        // found), retrying for the rest of the connect budget cannot help.
        if (isTerminalPortForwardFailure(tunnel.lastHandshakeError)) {
          throw new SandboxConnectionError(
            "sandboxd port-forward was rejected by the apiserver",
            "port_forward",
            { cause: tunnel.lastHandshakeError },
          );
        }
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
    opts?: RunOptions,
  ): Promise<ExecutionResult> {
    const timeoutMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    const startedAt = Date.now();
    return this.operate<ExecutionResult>(
      "command.run",
      timeoutMs,
      opts?.signal,
      (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.command.executable", "sh");
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
        return gen.process.run(command, remainingMs, signal);
      },
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
