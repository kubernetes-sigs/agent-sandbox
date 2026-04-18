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

import * as net from "node:net";
import { ChildProcess, spawn } from "node:child_process";
import * as k8s from "@kubernetes/client-node";

import type { RequestFn } from "./types.js";
import { CommandExecutor } from "./commands/index.js";
import { Filesystem } from "./files/index.js";
import {
  BACKOFF_FACTOR,
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  CLEANUP_TIMEOUT_MS,
  GATEWAY_API_GROUP,
  GATEWAY_API_VERSION,
  GATEWAY_PLURAL,
  GATEWAY_PROBE_INTERVAL_MS,
  GATEWAY_PROBE_TIMEOUT_MS,
  HEADER_REQUEST_ID,
  MAX_DRAIN_BYTES,
  MAX_ERROR_BODY_BYTES,
  MAX_GATEWAY_REWATCH,
  MAX_RECONNECT_ATTEMPTS,
  MAX_RETRIES,
  PER_ATTEMPT_TIMEOUT_MS,
  RETRY_STATUS_CODES,
} from "./constants.js";
import { TracerManager, withSpan } from "./trace-manager.js";
import type { Tracer } from "./trace-manager.js";
import {
  isK8s404,
  SandboxNotFoundError,
  SandboxNotReadyError,
  SandboxPortForwardError,
  SandboxRequestError,
  SandboxTimeoutError,
} from "./exceptions.js";
import { readBoundedErrorBody } from "./response-utils.js";

/**
 * Sleeps for `ms` milliseconds, aborting early if `signal` fires.
 * Throws the signal's reason (or a generic AbortError) when interrupted.
 */
async function sleepWithSignal(
  ms: number,
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) {
    throw (
      signal.reason ??
      new DOMException("The operation was aborted.", "AbortError")
    );
  }
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(
          signal.reason ??
            new DOMException("The operation was aborted.", "AbortError"),
        );
      },
      { once: true },
    );
  });
}

/**
 * Fetches a URL with retry logic.
 *
 * - maxRetries controls total attempt count (1 = no retry, 5 = up to 4 retries).
 * - overallSignal is an optional AbortSignal that caps the entire operation.
 *   When it fires, the retry loop stops immediately without further attempts.
 * - externalSignal is an optional caller-supplied AbortSignal. When it fires,
 *   the in-flight attempt is aborted and the loop exits immediately WITHOUT
 *   retrying, matching Go's context cancellation semantics.
 * - Each attempt gets its own per-attempt timeout (perAttemptTimeoutMs,
 *   default PER_ATTEMPT_TIMEOUT_MS).
 * - Before retrying a 5xx response, the response body is drained to allow
 *   the underlying TCP connection to be reused.
 */
async function fetchWithRetry(
  url: string,
  options: RequestInit,
  maxRetries: number = MAX_RETRIES,
  backoffFactor: number = BACKOFF_FACTOR,
  retryStatusCodes: number[] = RETRY_STATUS_CODES,
  overallSignal?: AbortSignal,
  externalSignal?: AbortSignal,
  perAttemptTimeoutMs: number = PER_ATTEMPT_TIMEOUT_MS,
): Promise<Response> {
  const attempts = Math.max(1, maxRetries);
  let lastError: Error | null = null;

  const throwExternalAbort = (cause?: unknown): never => {
    throw new SandboxRequestError("Request aborted by caller.", {
      cause: cause ?? externalSignal?.reason,
    });
  };

  // Composite cancel signal used to interrupt backoff sleeps when either
  // the overall deadline fires or the caller aborts.
  const cancelSignals = [overallSignal, externalSignal].filter(
    (s): s is AbortSignal => s !== undefined,
  );
  const cancelSignal =
    cancelSignals.length === 0
      ? undefined
      : cancelSignals.length === 1
        ? cancelSignals[0]
        : AbortSignal.any(cancelSignals);

  for (let attempt = 0; attempt < attempts; attempt++) {
    // Check external cancellation first: callers expect immediate exit,
    // not a retry, when they abort.
    if (externalSignal?.aborted) {
      throwExternalAbort();
    }
    // Check overall deadline before starting (and before sleeping)
    if (overallSignal?.aborted) {
      throw new SandboxTimeoutError(
        "Request timed out (overall deadline exceeded).",
      );
    }

    // use manual setTimeout so fake timers can control per-attempt timeout
    const attemptController = new AbortController();
    const attemptTimer = setTimeout(
      () =>
        attemptController.abort(
          new DOMException("per-attempt timeout", "TimeoutError"),
        ),
      perAttemptTimeoutMs,
    );
    const composedSignals = [
      overallSignal,
      externalSignal,
      attemptController.signal,
    ].filter((s): s is AbortSignal => s !== undefined);
    const attemptSignal =
      composedSignals.length === 1
        ? composedSignals[0]
        : AbortSignal.any(composedSignals);

    let response: Response;
    try {
      try {
        response = await fetch(url, { ...options, signal: attemptSignal });
      } finally {
        // stop per-attempt timer once headers are received
        clearTimeout(attemptTimer);
      }

      // If the external signal fired just as fetch resolved, discard the
      // response and bail out without retrying.
      if (externalSignal?.aborted) {
        response.body?.cancel().catch(() => {});
        throwExternalAbort();
      }

      // race condition guard: per-attempt timer fired just as fetch resolved
      if (attemptController.signal.aborted && !overallSignal?.aborted) {
        response.body?.cancel().catch(() => {});
        lastError = new Error(
          "per-attempt timer race — discarding response and retrying",
        );
        if (attempt < attempts - 1) {
          const delay = backoffFactor * Math.pow(2, attempt) * 1000;
          await sleepWithSignal(delay, cancelSignal);
        }
        continue;
      }

      if (
        attempt < attempts - 1 &&
        retryStatusCodes.includes(response.status)
      ) {
        const delay = backoffFactor * Math.pow(2, attempt) * 1000;
        console.debug(
          `Request to ${url} returned ${response.status}, retrying in ${delay}ms (attempt ${
            attempt + 1
          }/${attempts})...`,
        );
        // bounded drain to allow TCP connection reuse
        try {
          const reader = response.body?.getReader();
          if (reader) {
            let total = 0;
            while (total < MAX_DRAIN_BYTES) {
              const { done, value } = await reader.read();
              if (done) break;
              total += value.byteLength;
            }
            await reader.cancel();
          }
        } catch {
          // best-effort
        }
        await sleepWithSignal(delay, cancelSignal);
        continue;
      }
      return response;
    } catch (err) {
      lastError = err instanceof Error ? err : new Error(String(err));

      // Caller-initiated abort: surface as SandboxRequestError without retrying.
      if (externalSignal?.aborted) {
        throwExternalAbort(err);
      }

      // If the overall deadline fired, throw SandboxTimeoutError
      if (overallSignal?.aborted) {
        throw new SandboxTimeoutError(
          "Request timed out (overall deadline exceeded).",
        );
      }

      // Any other AbortError (per-attempt timeout) is treated as a transient failure
      if (attempt < attempts - 1) {
        const delay = backoffFactor * Math.pow(2, attempt) * 1000;
        console.debug(
          `Request to ${url} failed: ${lastError.message}, retrying in ${delay}ms (attempt ${
            attempt + 1
          }/${attempts})...`,
        );
        try {
          await sleepWithSignal(delay, cancelSignal);
        } catch (sleepErr) {
          if (externalSignal?.aborted) {
            throwExternalAbort(sleepErr);
          }
          if (overallSignal?.aborted) {
            throw new SandboxTimeoutError(
              "Request timed out (overall deadline exceeded).",
            );
          }
          throw sleepErr;
        }
      }
    }
  }
  throw lastError ?? new Error("Request failed after retries");
}

function formatGatewayAddress(addr: string): string {
  if (/[/?#@]/.test(addr)) {
    throw new SandboxNotReadyError(
      `Gateway address contains invalid characters: "${addr}"`,
    );
  }
  if (net.isIP(addr) === 6) {
    return `[${addr}]`;
  }
  return addr;
}

function getFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (addr && typeof addr !== "string") {
        const port = addr.port;
        server.close(() => resolve(port));
      } else {
        server.close(() => reject(new Error("Failed to get free port")));
      }
    });
    server.on("error", reject);
  });
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
  annotations: Record<string, string>;
  serverPort: number;
  apiUrl?: string;
  gatewayName?: string;
  gatewayNamespace?: string;
  gatewayReadyTimeout?: number;
  portForwardReadyTimeout?: number;
  perAttemptTimeoutMs?: number;
  kubeConfig: k8s.KubeConfig;
  customObjectsApi: k8s.CustomObjectsApi;
  traceServiceName: string;
  tracer: Tracer | null;
  tracingManager: TracerManager | null;
}

/**
 * Represents a connection to a single running Sandbox instance.
 * Obtain instances via SandboxClient.createSandbox() or getSandbox().
 */
export class Sandbox {
  readonly claimName: string;
  readonly sandboxName: string;
  readonly podName: string;
  readonly namespace: string;

  protected readonly serverPort: number;
  protected readonly traceServiceName: string;
  protected readonly tracer: Tracer | null;
  protected readonly tracingManager: TracerManager | null;
  protected readonly kubeConfig: k8s.KubeConfig;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;

  private readonly annotations: Record<string, string>;
  private readonly apiUrl: string | undefined;
  private readonly gatewayName: string | undefined;
  private readonly gatewayNamespace: string;
  private readonly gatewayReadyTimeout: number;
  private readonly portForwardReadyTimeout: number;
  private readonly perAttemptTimeoutMs: number;

  protected baseUrl: string | undefined;
  protected portForwardProcess: ChildProcess | null = null;
  private _isClosed = false;
  private _commands: CommandExecutor | null;
  private _files: Filesystem | null;
  private _inflightCount = 0;
  private _drainResolvers: Array<() => void> = [];

  constructor(init: SandboxInit) {
    this.claimName = init.claimName;
    this.sandboxName = init.sandboxName;
    this.podName = init.podName;
    this.namespace = init.namespace;
    this.annotations = init.annotations;
    this.serverPort = init.serverPort;
    this.apiUrl = init.apiUrl;
    this.gatewayName = init.gatewayName;
    this.gatewayNamespace = init.gatewayNamespace ?? "default";
    this.gatewayReadyTimeout = init.gatewayReadyTimeout ?? 180;
    this.portForwardReadyTimeout = init.portForwardReadyTimeout ?? 30;
    this.perAttemptTimeoutMs =
      init.perAttemptTimeoutMs ?? PER_ATTEMPT_TIMEOUT_MS;
    this.kubeConfig = init.kubeConfig;
    this.customObjectsApi = init.customObjectsApi;
    this.traceServiceName = init.traceServiceName;
    this.tracer = init.tracer;
    this.tracingManager = init.tracingManager;

    const requestFn: RequestFn = (method, endpoint, options) =>
      this.request(method, endpoint, options);
    const getTracer = () => this.tracer;
    const getParentContext = () => this.tracingManager?.parentContext ?? null;
    this._commands = new CommandExecutor(
      requestFn,
      getTracer,
      this.traceServiceName,
      getParentContext,
    );
    this._files = new Filesystem(
      requestFn,
      getTracer,
      this.traceServiceName,
      getParentContext,
    );
  }

  /**
   * Returns true if the sandbox is connected and not yet closed.
   */
  get isActive(): boolean {
    return !this._isClosed && this._commands !== null;
  }

  get commands(): CommandExecutor {
    if (!this._commands) {
      throw new SandboxNotReadyError("Sandbox connection has been closed.");
    }
    return this._commands;
  }

  get files(): Filesystem {
    if (!this._files) {
      throw new SandboxNotReadyError("Sandbox connection has been closed.");
    }
    return this._files;
  }

  /**
   * Establishes the transport connection (direct URL, gateway, or port-forward).
   * Called by SandboxClient after constructing the Sandbox instance.
   */
  async connect(): Promise<void> {
    if (this.apiUrl) {
      this.baseUrl = this.apiUrl;
      console.info(`Using configured API URL: ${this.baseUrl}`);
    } else if (this.gatewayName) {
      await this.waitForGatewayIp();
    } else {
      await this.startAndWaitForPortForward();
    }
  }

  /**
   * Stops the port-forward process (if any) and clears local state.
   * Does NOT delete the SandboxClaim from Kubernetes.
   * Use this when a reconnect fails and you want to release local resources
   * without destroying the live claim.
   */
  async closeLocal(): Promise<void> {
    if (this.portForwardProcess) {
      try {
        console.info("Stopping port-forwarding...");
        this.portForwardProcess.kill("SIGTERM");
        await new Promise<void>((resolve) => {
          const timeout = setTimeout(() => {
            this.portForwardProcess?.kill("SIGKILL");
            resolve();
          }, 2000);
          this.portForwardProcess?.on("exit", () => {
            clearTimeout(timeout);
            resolve();
          });
        });
      } catch (err) {
        console.error(`Failed to stop port-forwarding: ${err}`);
      }
      this.portForwardProcess = null;
    }

    this._commands = null;
    this._files = null;
    this._isClosed = true;

    if (this.tracingManager) {
      try {
        this.tracingManager.endLifecycleSpan();
      } catch (err) {
        console.error(`Failed to end tracing span: ${err}`);
      }
    }
  }

  /**
   * Terminates the port-forward process (if any) and deletes the SandboxClaim.
   */
  async close(): Promise<void> {
    // Prevent new requests immediately so in-flight count stabilises
    this._isClosed = true;

    // Drain in-flight requests; give up after CLEANUP_TIMEOUT_MS so close() is bounded
    await Promise.race([
      this.drainInflight(),
      new Promise<void>((resolve) => setTimeout(resolve, CLEANUP_TIMEOUT_MS)),
    ]);

    // Kill port-forward and clear local resources (also sets _isClosed = true, idempotent)
    await this.closeLocal();

    if (this.claimName) {
      console.info(`Deleting SandboxClaim: ${this.claimName}`);
      try {
        await Promise.race([
          this.customObjectsApi.deleteNamespacedCustomObject({
            group: CLAIM_API_GROUP,
            version: CLAIM_API_VERSION,
            namespace: this.namespace,
            plural: CLAIM_PLURAL_NAME,
            name: this.claimName,
          }),
          new Promise<never>((_, reject) =>
            setTimeout(
              () =>
                reject(
                  new Error(
                    `SandboxClaim cleanup timed out after ${CLEANUP_TIMEOUT_MS}ms`,
                  ),
                ),
              CLEANUP_TIMEOUT_MS,
            ),
          ),
        ]);
      } catch (err: unknown) {
        if (!isK8s404(err)) {
          console.error(`Error deleting sandbox claim: ${err}`);
        }
      }
    }
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  private extractGatewayAddress(
    obj: Record<string, unknown> | undefined,
  ): string | undefined {
    const status = (obj?.status as Record<string, unknown>) ?? {};
    const addresses = (status.addresses as Array<Record<string, string>>) ?? [];
    return addresses[0]?.value;
  }

  private async waitForGatewayIp(): Promise<void> {
    const fn = async () => {
      if (this.baseUrl) {
        console.info(`Using configured API URL: ${this.baseUrl}`);
        return;
      }

      console.info(
        `Waiting for Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}'...`,
      );

      const startTime = Date.now();
      const timeoutMs = this.gatewayReadyTimeout * 1000;

      // Helper: (re-)fetch the gateway object and return its address if available
      const getExistingAddress = async (): Promise<string | undefined> => {
        try {
          const response =
            await this.customObjectsApi.getNamespacedCustomObject({
              group: GATEWAY_API_GROUP,
              version: GATEWAY_API_VERSION,
              namespace: this.gatewayNamespace,
              plural: GATEWAY_PLURAL,
              name: this.gatewayName!,
            });
          const gatewayObj =
            (response as { body?: Record<string, unknown> }).body ??
            (response as Record<string, unknown>);
          return this.extractGatewayAddress(gatewayObj);
        } catch (err) {
          if (!isK8s404(err)) throw err;
          return undefined;
        }
      };

      // Initial GET: resolve immediately if the gateway already has an address
      const existingAddress = await getExistingAddress();
      if (existingAddress) {
        this.baseUrl = `http://${formatGatewayAddress(existingAddress)}`;
        console.info(
          `Gateway is already ready. Base URL set to: ${this.baseUrl}`,
        );
        await this.probeGatewayConnectivity(
          existingAddress,
          timeoutMs - (Date.now() - startTime),
        );
        return;
      }

      // Watch loop: on clean-close (err === null), re-list and re-watch
      for (
        let watchAttempt = 0;
        watchAttempt < MAX_GATEWAY_REWATCH;
        watchAttempt++
      ) {
        const elapsed = Date.now() - startTime;
        if (elapsed >= timeoutMs) {
          throw new SandboxNotReadyError(
            `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' did not get ` +
              `an IP within ${this.gatewayReadyTimeout} seconds.`,
          );
        }
        const remainingMs = timeoutMs - elapsed;

        const result = await this.watchGatewayOnce(remainingMs);

        if (result.type === "resolved") {
          this.baseUrl = `http://${formatGatewayAddress(result.address)}`;
          console.info(`Gateway is ready. Base URL set to: ${this.baseUrl}`);
          await this.probeGatewayConnectivity(
            result.address,
            timeoutMs - (Date.now() - startTime),
          );
          return;
        }

        if (result.type === "error") {
          throw result.error;
        }

        // result.type === "clean-close": the watch stream ended normally.
        // Re-list before re-watching to avoid missing an update that arrived
        // between the stream closing and the next watch starting.
        console.debug(
          `Gateway watch closed cleanly, re-listing and re-watching (attempt ${watchAttempt + 1})...`,
        );
        const relistAddress = await getExistingAddress();
        if (relistAddress) {
          this.baseUrl = `http://${formatGatewayAddress(relistAddress)}`;
          console.info(
            `Gateway is ready (after re-list). Base URL set to: ${this.baseUrl}`,
          );
          await this.probeGatewayConnectivity(
            relistAddress,
            timeoutMs - (Date.now() - startTime),
          );
          return;
        }
      }

      throw new SandboxNotReadyError(
        `Gateway '${this.gatewayName}' watch loop exhausted after ${MAX_GATEWAY_REWATCH} reconnects.`,
      );
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "wait_for_gateway",
      fn,
      this.tracingManager?.parentContext,
    );
  }

  /**
   * Runs a single watch against the Gateway resource and returns a discriminated result:
   * - "resolved": an IP address was found
   * - "error":    a fatal error occurred (gateway deleted, watch error, timeout)
   * - "clean-close": the watch stream closed normally; caller should re-list and re-watch
   */
  private watchGatewayOnce(
    timeoutMs: number,
  ): Promise<
    | { type: "resolved"; address: string }
    | { type: "error"; error: Error }
    | { type: "clean-close" }
  > {
    return new Promise((resolve) => {
      const watcher = new k8s.Watch(this.kubeConfig);
      let abortController: AbortController | undefined;
      let settled = false;
      let timer: ReturnType<typeof setTimeout>;

      const cleanup = () => {
        settled = true;
        clearTimeout(timer);
        try {
          abortController?.abort();
        } catch {
          // ignore
        }
      };

      timer = setTimeout(() => {
        cleanup();
        resolve({
          type: "error",
          error: new SandboxNotReadyError(
            `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' did not get ` +
              `an IP within the allotted time.`,
          ),
        });
      }, timeoutMs);

      watcher
        .watch(
          `/apis/${GATEWAY_API_GROUP}/${GATEWAY_API_VERSION}/namespaces/${this.gatewayNamespace}/${GATEWAY_PLURAL}`,
          { fieldSelector: `metadata.name=${this.gatewayName}` },
          (type: string, obj: Record<string, unknown>) => {
            if (settled) return;
            if (type === "ADDED" || type === "MODIFIED") {
              const ipAddress = this.extractGatewayAddress(obj);
              if (ipAddress) {
                cleanup();
                resolve({ type: "resolved", address: ipAddress });
              }
            } else if (type === "DELETED") {
              cleanup();
              resolve({
                type: "error",
                error: new SandboxNotFoundError(
                  `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' was deleted while waiting for it to become ready.`,
                ),
              });
            }
          },
          (err) => {
            cleanup();
            if (err && !(err instanceof Error && err.name === "AbortError")) {
              resolve({
                type: "error",
                error: err instanceof Error ? err : new Error(String(err)),
              });
            } else {
              // err === null or AbortError from our own cleanup: clean close
              resolve({ type: "clean-close" });
            }
          },
        )
        .then((ac) => {
          if (settled) {
            ac.abort();
          } else {
            abortController = ac;
          }
        })
        .catch((err: unknown) => {
          // watcher.watch() itself rejected (auth error, network down, etc.)
          if (!settled) {
            cleanup();
            resolve({
              type: "error",
              error: err instanceof Error ? err : new Error(String(err)),
            });
          }
        });
    });
  }

  /**
   * After the gateway reports an IP address, the underlying proxy (e.g. Envoy)
   * may not yet be accepting TCP connections. This method polls until a TCP
   * connect succeeds or the remaining timeout budget is exhausted.
   */
  private async probeGatewayConnectivity(
    address: string,
    remainingMs: number,
  ): Promise<void> {
    const host = formatGatewayAddress(address);
    // Derive port from the base URL (defaults to 80 for http).
    const port = 80;
    const deadline = Date.now() + Math.max(remainingMs, 0);
    const probeTimeoutMs = Math.min(
      GATEWAY_PROBE_TIMEOUT_MS,
      Math.max(remainingMs, 0),
    );

    console.info(
      `Probing gateway connectivity at ${host}:${port} (timeout ${probeTimeoutMs}ms)...`,
    );

    const startTime = Date.now();
    while (Date.now() < deadline) {
      const connected = await new Promise<boolean>((resolve) => {
        const sock = net.createConnection({ host, port, timeout: 500 }, () => {
          sock.destroy();
          resolve(true);
        });
        sock.on("error", () => {
          sock.destroy();
          resolve(false);
        });
        sock.on("timeout", () => {
          sock.destroy();
          resolve(false);
        });
      });

      if (connected) {
        console.info(
          `Gateway at ${host}:${port} is accepting connections ` +
            `(took ${Date.now() - startTime}ms).`,
        );
        return;
      }

      await new Promise((resolve) =>
        setTimeout(resolve, GATEWAY_PROBE_INTERVAL_MS),
      );
    }

    throw new SandboxNotReadyError(
      `Gateway at ${host}:${port} did not accept connections within the timeout.`,
    );
  }

  private async startAndWaitForPortForward(): Promise<void> {
    const fn = async () => {
      const localPort = await getFreePort();
      const routerSvc = "svc/sandbox-router-svc";

      console.info(
        `Starting Dev Mode tunnel: localhost:${localPort} -> ${routerSvc}:8080...`,
      );

      this.portForwardProcess = spawn(
        "kubectl",
        ["port-forward", routerSvc, `${localPort}:8080`, "-n", this.namespace],
        { stdio: ["ignore", "pipe", "pipe"] },
      );

      // capture spawn errors (e.g. kubectl not found)
      let rejectSpawnError!: (err: Error) => void;
      const spawnErrorPromise = new Promise<never>((_, reject) => {
        rejectSpawnError = reject;
      });
      this.portForwardProcess.on("error", (err: Error) => {
        rejectSpawnError(
          new SandboxPortForwardError(
            `Failed to spawn kubectl: ${err.message}`,
            { cause: err },
          ),
        );
      });

      console.info("Waiting for port-forwarding to be ready...");
      const startTime = Date.now();
      const timeoutMs = this.portForwardReadyTimeout * 1000;

      // use try-finally so closeLocal() is never called on timeout
      try {
        while (Date.now() - startTime < timeoutMs) {
          if (
            this.portForwardProcess.exitCode !== null ||
            this.portForwardProcess.signalCode !== null
          ) {
            throw new SandboxPortForwardError(
              `Tunnel crashed: port-forward process exited with code ${this.portForwardProcess.exitCode}, signal ${this.portForwardProcess.signalCode}`,
            );
          }

          const connected = await Promise.race([
            new Promise<boolean>((resolve) => {
              const sock = net.createConnection(
                { host: "127.0.0.1", port: localPort, timeout: 100 },
                () => {
                  sock.destroy();
                  resolve(true);
                },
              );
              sock.on("error", () => {
                sock.destroy();
                resolve(false);
              });
              sock.on("timeout", () => {
                sock.destroy();
                resolve(false);
              });
            }),
            spawnErrorPromise,
          ]);

          if (connected) {
            this.baseUrl = `http://127.0.0.1:${localPort}`;
            console.info(
              `Dev Mode ready. Tunneled to Router at ${this.baseUrl}`,
            );
            await new Promise((resolve) => setTimeout(resolve, 500));
            return;
          }

          await new Promise((resolve) => setTimeout(resolve, 500));
        }

        throw new SandboxPortForwardError(
          "Failed to establish tunnel to Router Service.",
        );
      } finally {
        // cleanup transport only on failure; do NOT touch _isClosed
        if (!this.baseUrl) {
          await this.stopPortForward();
        }
      }
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "dev_mode_tunnel",
      fn,
      this.tracingManager?.parentContext,
    );
  }

  protected async request(
    method: string,
    endpoint: string,
    options: {
      body?: BodyInit | null;
      headers?: Record<string, string>;
      timeout?: number;
      maxRetries?: number;
      signal?: AbortSignal;
      perAttemptTimeoutMs?: number;
    } = {},
  ): Promise<Response> {
    // reject pre-captured references used after close()
    if (this._isClosed) {
      throw new SandboxNotReadyError("Sandbox connection has been closed.");
    }

    this._inflightCount++;
    try {
      // if reconnect is in flight, await the shared promise
      if (this._reconnectPromise) {
        await this._reconnectPromise; // may throw SandboxPortForwardError
      } else if (
        this.portForwardProcess &&
        (this.portForwardProcess.exitCode !== null ||
          this.portForwardProcess.signalCode !== null)
      ) {
        await this.reconnect();
      }

      if (!this.baseUrl) {
        throw new SandboxNotReadyError(
          "Sandbox is not ready for communication.",
        );
      }

      const url = `${this.baseUrl!.replace(/\/+$/, "")}/${endpoint.replace(
        /^\/+/,
        "",
      )}`;

      // Correlation ID shared across all retry attempts for a single
      // request cycle (matches Go client's connector.SendRequest).
      // Respect a caller-supplied X-Request-ID if present (case-insensitive).
      const existingRequestId = options.headers
        ? Object.entries(options.headers).find(
            ([k]) => k.toLowerCase() === HEADER_REQUEST_ID.toLowerCase(),
          )?.[1]
        : undefined;
      const requestId = existingRequestId ?? globalThis.crypto.randomUUID();

      const headers: Record<string, string> = {
        ...options.headers,
        "X-Sandbox-ID": this.sandboxName,
        "X-Sandbox-Namespace": this.namespace,
        "X-Sandbox-Port": String(this.serverPort),
        [HEADER_REQUEST_ID]: requestId,
      };

      const fetchOptions: RequestInit = {
        method,
        headers,
        body: options.body,
      };

      // The overall timeout signal caps the entire operation including retries
      const overallSignal = options.timeout
        ? AbortSignal.timeout(options.timeout * 1000)
        : undefined;

      try {
        const response = await fetchWithRetry(
          url,
          fetchOptions,
          options.maxRetries ?? MAX_RETRIES,
          BACKOFF_FACTOR,
          RETRY_STATUS_CODES,
          overallSignal,
          options.signal,
          options.perAttemptTimeoutMs ?? this.perAttemptTimeoutMs,
        );
        if (!response.ok) {
          // Bounded read so a hostile server cannot force arbitrarily large
          // payloads into memory via the error path.
          const body = await readBoundedErrorBody(
            response,
            MAX_ERROR_BODY_BYTES,
          );
          throw new SandboxRequestError(
            `Request failed with status ${response.status}: ${response.statusText}` +
              (body ? ` — ${body}` : ""),
            {
              statusCode: response.status,
              response,
              body,
              operation: `${method} ${endpoint}`,
            },
          );
        }
        return response;
      } catch (err) {
        if (err instanceof SandboxRequestError) throw err;
        if (err instanceof SandboxTimeoutError) throw err;

        if (
          this.portForwardProcess &&
          (this.portForwardProcess.exitCode !== null ||
            this.portForwardProcess.signalCode !== null)
        ) {
          throw new SandboxPortForwardError(
            `Kubectl Port-Forward crashed DURING request! ` +
              `Exit code: ${this.portForwardProcess.exitCode}, signal: ${this.portForwardProcess.signalCode}`,
            { cause: err },
          );
        }

        console.error(`Request to gateway router failed: ${err}`);
        throw new SandboxRequestError(
          `Failed to communicate with the sandbox via the gateway at ${url}.`,
          { cause: err },
        );
      }
    } finally {
      this._inflightCount--;
      if (this._inflightCount === 0 && this._drainResolvers.length > 0) {
        for (const resolve of this._drainResolvers) resolve();
        this._drainResolvers = [];
      }
    }
  }

  /**
   * Stops the port-forward process and resets transport state.
   * Unlike closeLocal(), this does NOT mark the Sandbox as closed.
   */
  private async stopPortForward(): Promise<void> {
    if (!this.portForwardProcess) return;
    try {
      this.portForwardProcess.kill("SIGTERM");
      const proc = this.portForwardProcess;
      await new Promise<void>((resolve) => {
        const timer = setTimeout(() => {
          proc.kill("SIGKILL");
          resolve();
        }, 2000);
        proc.on("exit", () => {
          clearTimeout(timer);
          resolve();
        });
      });
    } catch (err) {
      console.error(`Failed to stop port-forward process: ${err}`);
    } finally {
      this.portForwardProcess = null;
      this.baseUrl = undefined;
    }
  }

  protected _reconnectPromise: Promise<void> | null = null;

  private drainInflight(): Promise<void> {
    if (this._inflightCount === 0) return Promise.resolve();
    return new Promise<void>((resolve) => {
      this._drainResolvers.push(resolve);
    });
  }

  /**
   * Re-establishes the port-forward tunnel.
   * Only applies to port-forward mode (no apiUrl, no gatewayName).
   * Concurrent callers share the same promise instead of starting a second reconnect.
   */
  private async reconnect(): Promise<void> {
    // Gateway or direct-URL modes do not use port-forward
    if (this.apiUrl || this.gatewayName) return;

    // concurrent callers share the same promise
    if (this._reconnectPromise) {
      return this._reconnectPromise;
    }

    const doReconnect = async (): Promise<void> => {
      await this.stopPortForward();
      for (let i = 1; i <= MAX_RECONNECT_ATTEMPTS; i++) {
        try {
          console.info(
            `Port-forward reconnect attempt ${i}/${MAX_RECONNECT_ATTEMPTS}...`,
          );
          await this.startAndWaitForPortForward();
          console.info("Port-forward reconnect succeeded.");
          return;
        } catch (err) {
          console.warn(`Port-forward reconnect attempt ${i} failed: ${err}`);
          if (i < MAX_RECONNECT_ATTEMPTS) {
            await new Promise((resolve) => setTimeout(resolve, 1000 * i));
          }
        }
      }
      throw new SandboxPortForwardError(
        `Failed to re-establish port-forward after ${MAX_RECONNECT_ATTEMPTS} attempts.`,
      );
    };

    this._reconnectPromise = doReconnect().finally(() => {
      this._reconnectPromise = null;
    });

    return this._reconnectPromise;
  }
}
