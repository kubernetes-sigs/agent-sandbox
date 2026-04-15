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
  GATEWAY_API_GROUP,
  GATEWAY_API_VERSION,
  GATEWAY_PLURAL,
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
} from "./exceptions.js";

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
 * - Each attempt gets its own per-attempt timeout (PER_ATTEMPT_TIMEOUT_MS).
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
): Promise<Response> {
  const attempts = Math.max(1, maxRetries);
  let lastError: Error | null = null;

  for (let attempt = 0; attempt < attempts; attempt++) {
    // Check overall deadline before starting (and before sleeping)
    if (overallSignal?.aborted) {
      throw (
        overallSignal.reason ??
        new DOMException("The operation was aborted.", "AbortError")
      );
    }

    // Each attempt gets its own timeout; combine with the overall signal if present
    const perAttemptSignal = AbortSignal.timeout(PER_ATTEMPT_TIMEOUT_MS);
    const attemptSignal = overallSignal
      ? AbortSignal.any([overallSignal, perAttemptSignal])
      : perAttemptSignal;

    try {
      const response = await fetch(url, { ...options, signal: attemptSignal });

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
        // Drain the body (up to MAX_DRAIN_BYTES) to allow TCP connection reuse
        try {
          await response.text();
        } catch {
          // Ignore drain errors; the retry is more important
        }
        await sleepWithSignal(delay, overallSignal);
        continue;
      }
      return response;
    } catch (err) {
      lastError = err instanceof Error ? err : new Error(String(err));

      // If the overall deadline fired, propagate immediately — no more retries
      if (overallSignal?.aborted) {
        throw overallSignal.reason ?? lastError;
      }

      // Any other AbortError (per-attempt timeout) is treated as a transient failure
      if (attempt < attempts - 1) {
        const delay = backoffFactor * Math.pow(2, attempt) * 1000;
        console.debug(
          `Request to ${url} failed: ${lastError.message}, retrying in ${delay}ms (attempt ${
            attempt + 1
          }/${attempts})...`,
        );
        await sleepWithSignal(delay, overallSignal);
      }
    }
  }
  throw lastError ?? new Error("Request failed after retries");
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

  protected baseUrl: string | undefined;
  protected portForwardProcess: ChildProcess | null = null;
  private _isClosed = false;
  private _commands: CommandExecutor | null;
  private _files: Filesystem | null;

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
    await this.closeLocal();

    if (this.claimName) {
      console.info(`Deleting SandboxClaim: ${this.claimName}`);
      try {
        await this.customObjectsApi.deleteNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: this.namespace,
          plural: CLAIM_PLURAL_NAME,
          name: this.claimName,
        });
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
        this.baseUrl = `http://${existingAddress}`;
        console.info(
          `Gateway is already ready. Base URL set to: ${this.baseUrl}`,
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
          this.baseUrl = `http://${result.address}`;
          console.info(`Gateway is ready. Base URL set to: ${this.baseUrl}`);
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
          this.baseUrl = `http://${relistAddress}`;
          console.info(
            `Gateway is ready (after re-list). Base URL set to: ${this.baseUrl}`,
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
        });
    });
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

      console.info("Waiting for port-forwarding to be ready...");
      const startTime = Date.now();
      const timeoutMs = this.portForwardReadyTimeout * 1000;

      while (Date.now() - startTime < timeoutMs) {
        if (
          this.portForwardProcess.exitCode !== null ||
          this.portForwardProcess.signalCode !== null
        ) {
          throw new SandboxPortForwardError(
            `Tunnel crashed: port-forward process exited with code ${this.portForwardProcess.exitCode}, signal ${this.portForwardProcess.signalCode}`,
          );
        }

        const connected = await new Promise<boolean>((resolve) => {
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
        });

        if (connected) {
          this.baseUrl = `http://127.0.0.1:${localPort}`;
          console.info(`Dev Mode ready. Tunneled to Router at ${this.baseUrl}`);
          await new Promise((resolve) => setTimeout(resolve, 500));
          return;
        }

        await new Promise((resolve) => setTimeout(resolve, 500));
      }

      await this.closeLocal();
      throw new SandboxPortForwardError(
        "Failed to establish tunnel to Router Service.",
      );
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
    } = {},
  ): Promise<Response> {
    if (!this.baseUrl) {
      throw new SandboxNotReadyError("Sandbox is not ready for communication.");
    }

    // If the port-forward process has died, attempt to reconnect before failing
    if (
      this.portForwardProcess &&
      (this.portForwardProcess.exitCode !== null ||
        this.portForwardProcess.signalCode !== null)
    ) {
      await this.reconnect();
    }

    const url = `${this.baseUrl!.replace(/\/+$/, "")}/${endpoint.replace(
      /^\/+/,
      "",
    )}`;

    const headers: Record<string, string> = {
      ...options.headers,
      "X-Sandbox-ID": this.sandboxName,
      "X-Sandbox-Namespace": this.namespace,
      "X-Sandbox-Port": String(this.serverPort),
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
      );
      if (!response.ok) {
        // Read the body so callers can inspect the failure payload
        let body = "";
        try {
          body = (await response.text()).slice(0, MAX_ERROR_BODY_BYTES);
        } catch {
          // Ignore body-read errors
        }
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

  private _reconnecting = false;

  /**
   * Re-establishes the port-forward tunnel.
   * Only applies to port-forward mode (no apiUrl, no gatewayName).
   * Concurrent callers wait for the in-progress reconnect instead of
   * starting a second one.
   */
  private async reconnect(): Promise<void> {
    // Gateway or direct-URL modes do not use port-forward
    if (this.apiUrl || this.gatewayName) return;

    if (this._reconnecting) {
      // Another request already started a reconnect; wait briefly and re-check
      await new Promise((resolve) => setTimeout(resolve, 2000));
      if (!this.baseUrl) {
        throw new SandboxPortForwardError(
          "Port-forward reconnect is in progress but did not complete in time.",
        );
      }
      return;
    }

    this._reconnecting = true;
    try {
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
    } finally {
      this._reconnecting = false;
    }
  }
}
