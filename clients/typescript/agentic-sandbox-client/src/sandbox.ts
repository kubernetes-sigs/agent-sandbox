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
  MAX_RETRIES,
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

async function fetchWithRetry(
  url: string,
  options: RequestInit,
  maxRetries: number = MAX_RETRIES,
  backoffFactor: number = BACKOFF_FACTOR,
  retryStatusCodes: number[] = RETRY_STATUS_CODES,
): Promise<Response> {
  let lastError: Error | null = null;
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    try {
      const response = await fetch(url, options);
      if (attempt < maxRetries && retryStatusCodes.includes(response.status)) {
        const delay = backoffFactor * Math.pow(2, attempt) * 1000;
        console.debug(
          `Request to ${url} returned ${response.status}, retrying in ${delay}ms (attempt ${
            attempt + 1
          }/${maxRetries})...`,
        );
        await new Promise((resolve) => setTimeout(resolve, delay));
        continue;
      }
      return response;
    } catch (err) {
      lastError = err instanceof Error ? err : new Error(String(err));
      if (attempt < maxRetries) {
        const delay = backoffFactor * Math.pow(2, attempt) * 1000;
        console.debug(
          `Request to ${url} failed: ${lastError.message}, retrying in ${delay}ms (attempt ${
            attempt + 1
          }/${maxRetries})...`,
        );
        await new Promise((resolve) => setTimeout(resolve, delay));
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
   * Terminates the port-forward process (if any) and deletes the SandboxClaim.
   */
  async close(): Promise<void> {
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

      try {
        const response = await this.customObjectsApi.getNamespacedCustomObject({
          group: GATEWAY_API_GROUP,
          version: GATEWAY_API_VERSION,
          namespace: this.gatewayNamespace,
          plural: GATEWAY_PLURAL,
          name: this.gatewayName!,
        });
        const gatewayObj =
          (response as { body?: Record<string, unknown> }).body ??
          (response as Record<string, unknown>);
        const existingAddress = this.extractGatewayAddress(gatewayObj);
        if (existingAddress) {
          this.baseUrl = `http://${existingAddress}`;
          console.info(
            `Gateway is already ready. Base URL set to: ${this.baseUrl}`,
          );
          return;
        }
      } catch (err) {
        if (!isK8s404(err)) {
          throw err;
        }
      }

      const watcher = new k8s.Watch(this.kubeConfig);
      const timeoutMs = this.gatewayReadyTimeout * 1000;

      await new Promise<void>((resolve, reject) => {
        let abortController: AbortController | undefined;
        let aborted = false;
        let timer: ReturnType<typeof setTimeout>;

        const cleanup = () => {
          aborted = true;
          clearTimeout(timer);
          if (abortController) {
            try {
              abortController.abort();
            } catch {
              // ignore
            }
          }
        };

        timer = setTimeout(() => {
          cleanup();
          reject(
            new SandboxNotReadyError(
              `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' did not get ` +
                `an IP within ${this.gatewayReadyTimeout} seconds.`,
            ),
          );
        }, timeoutMs);

        watcher
          .watch(
            `/apis/${GATEWAY_API_GROUP}/${GATEWAY_API_VERSION}/namespaces/${this.gatewayNamespace}/${GATEWAY_PLURAL}`,
            { fieldSelector: `metadata.name=${this.gatewayName}` },
            (type: string, obj: Record<string, unknown>) => {
              if (type === "ADDED" || type === "MODIFIED") {
                const ipAddress = this.extractGatewayAddress(obj);
                if (ipAddress) {
                  this.baseUrl = `http://${ipAddress}`;
                  console.info(
                    `Gateway is ready. Base URL set to: ${this.baseUrl}`,
                  );
                  cleanup();
                  resolve();
                }
              } else if (type === "DELETED") {
                cleanup();
                reject(
                  new SandboxNotFoundError(
                    `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' was deleted while waiting for it to become ready.`,
                  ),
                );
              }
            },
            (err) => {
              cleanup();
              if (err && !(err instanceof Error && err.name === "AbortError")) {
                reject(err);
              }
            },
          )
          .then((ac) => {
            if (aborted) {
              ac.abort();
            } else {
              abortController = ac;
            }
          });
      });
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "wait_for_gateway",
      fn,
      this.tracingManager?.parentContext,
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

      await this.close();
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
    } = {},
  ): Promise<Response> {
    if (!this.baseUrl) {
      throw new SandboxNotReadyError("Sandbox is not ready for communication.");
    }

    if (
      this.portForwardProcess &&
      (this.portForwardProcess.exitCode !== null ||
        this.portForwardProcess.signalCode !== null)
    ) {
      throw new SandboxPortForwardError(
        `Kubectl Port-Forward crashed BEFORE request! ` +
          `Exit code: ${this.portForwardProcess.exitCode}, signal: ${this.portForwardProcess.signalCode}`,
      );
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

    if (options.timeout) {
      fetchOptions.signal = AbortSignal.timeout(options.timeout * 1000);
    }

    try {
      const response = await fetchWithRetry(url, fetchOptions);
      if (!response.ok) {
        throw new SandboxRequestError(
          `Request failed with status ${response.status}: ${response.statusText}`,
          { statusCode: response.status, response },
        );
      }
      return response;
    } catch (err) {
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
}
