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

import * as crypto from "node:crypto";
import * as net from "node:net";
import { ChildProcess, spawn } from "node:child_process";
import * as k8s from "@kubernetes/client-node";

import type { RequestFn, SandboxOptions } from "./types.js";
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
  POD_NAME_ANNOTATION,
  RETRY_STATUS_CODES,
  SANDBOX_API_GROUP,
  SANDBOX_API_VERSION,
  SANDBOX_PLURAL_NAME,
} from "./constants.js";
import {
  getCurrentSpan,
  initializeTracer,
  TracerManager,
  withSpan,
} from "./trace-manager.js";
import type { Tracer } from "./trace-manager.js";

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

export class SandboxClient {
  protected traceServiceName: string;
  protected tracingManager: TracerManager | null = null;
  protected tracer: Tracer | null = null;

  protected templateName: string;
  protected namespace: string;
  protected gatewayName: string | undefined;
  protected gatewayNamespace: string;
  protected baseUrl: string | undefined;
  protected serverPort: number;
  protected sandboxReadyTimeout: number;
  protected gatewayReadyTimeout: number;
  protected portForwardReadyTimeout: number;
  protected enableTracing: boolean;

  protected portForwardProcess: ChildProcess | null = null;
  protected claimName: string | undefined;
  protected sandboxName: string | undefined;
  protected podName: string | undefined;
  protected annotations: Record<string, string> | undefined;

  private _commands: CommandExecutor | null = null;
  private _files: Filesystem | null = null;

  protected kubeConfig: k8s.KubeConfig;
  protected customObjectsApi: k8s.CustomObjectsApi;

  constructor(options: SandboxOptions) {
    this.templateName = options.templateName;
    this.namespace = options.namespace ?? "default";
    this.gatewayName = options.gatewayName;
    this.gatewayNamespace = options.gatewayNamespace ?? "default";
    this.baseUrl = options.apiUrl;
    this.serverPort = options.serverPort ?? 8888;
    this.sandboxReadyTimeout = options.sandboxReadyTimeout ?? 180;
    this.gatewayReadyTimeout = options.gatewayReadyTimeout ?? 180;
    this.portForwardReadyTimeout = options.portForwardReadyTimeout ?? 30;
    this.enableTracing = options.enableTracing ?? false;
    this.traceServiceName = options.traceServiceName ?? "sandbox-client";

    this.kubeConfig = new k8s.KubeConfig();
    this.kubeConfig.loadFromDefault();
    this.customObjectsApi = this.kubeConfig.makeApiClient(k8s.CustomObjectsApi);

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

  isReady(): boolean {
    return this.baseUrl !== undefined;
  }

  get commands(): CommandExecutor {
    if (!this._commands) {
      throw new Error("Sandbox connection has been closed.");
    }
    return this._commands;
  }

  get files(): Filesystem {
    if (!this._files) {
      throw new Error("Sandbox connection has been closed.");
    }
    return this._files;
  }

  async start(): Promise<this> {
    let traceContextStr = "";

    if (this.enableTracing) {
      await initializeTracer(this.traceServiceName);
      this.tracingManager = new TracerManager(this.traceServiceName);
      this.tracer = this.tracingManager.tracer;
      this.tracingManager.startLifecycleSpan();
      traceContextStr = this.tracingManager.getTraceContextJson();
    }

    await this.createClaim(traceContextStr);
    await this.waitForSandboxReady();

    if (this.baseUrl) {
      console.info(`Using configured API URL: ${this.baseUrl}`);
    } else if (this.gatewayName) {
      await this.waitForGatewayIp();
    } else {
      await this.startAndWaitForPortForward();
    }

    return this;
  }

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
        const is404 = err instanceof Error && err.message.includes("404");
        if (!is404) {
          console.error(`Error deleting sandbox claim: ${err}`);
        }
      }
    }

    this._commands = null;
    this._files = null;

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

  private async createClaim(traceContextStr: string = ""): Promise<void> {
    const fn = async () => {
      this.claimName = `sandbox-claim-${crypto.randomBytes(4).toString("hex")}`;

      const span = getCurrentSpan();
      if (span.isRecording()) {
        span.setAttribute("sandbox.claim.name", this.claimName);
      }

      const annotations: Record<string, string> = {};
      if (traceContextStr) {
        annotations["opentelemetry.io/trace-context"] = traceContextStr;
      }

      const manifest = {
        apiVersion: `${CLAIM_API_GROUP}/${CLAIM_API_VERSION}`,
        kind: "SandboxClaim",
        metadata: {
          name: this.claimName,
          annotations,
        },
        spec: {
          sandboxTemplateRef: { name: this.templateName },
        },
      };

      console.info(
        `Creating SandboxClaim '${this.claimName}' ` +
          `in namespace '${this.namespace}' ` +
          `using template '${this.templateName}'...`,
      );

      await this.customObjectsApi.createNamespacedCustomObject({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace: this.namespace,
        plural: CLAIM_PLURAL_NAME,
        body: manifest,
      });
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "create_claim",
      fn,
      this.tracingManager?.parentContext,
    );
  }

  private resolveSandboxName(timeoutMs: number): Promise<string> {
    if (!this.claimName) {
      return Promise.reject(
        new Error(
          "Cannot resolve sandbox name; a sandboxclaim has not been created.",
        ),
      );
    }

    console.info(`Resolving sandbox name from claim '${this.claimName}'...`);

    const watcher = new k8s.Watch(this.kubeConfig);

    return new Promise<string>((resolve, reject) => {
      let abortController: AbortController | undefined;
      let timer: ReturnType<typeof setTimeout>;

      const cleanup = () => {
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
          new Error(
            `Sandbox did not become ready within ${this.sandboxReadyTimeout} seconds.`,
          ),
        );
      }, timeoutMs);

      watcher
        .watch(
          `/apis/${CLAIM_API_GROUP}/${CLAIM_API_VERSION}/namespaces/${this.namespace}/${CLAIM_PLURAL_NAME}`,
          { fieldSelector: `metadata.name=${this.claimName}` },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ADDED" || type === "MODIFIED") {
              const status = (obj.status as Record<string, unknown>) ?? {};
              const sandboxStatus =
                (status.sandbox as Record<string, unknown>) ?? {};
              const name = sandboxStatus.name as string | undefined;
              if (name) {
                console.info(
                  `Resolved sandbox name '${name}' from claim status.`,
                );
                cleanup();
                resolve(name);
              }
            }
          },
          (err) => {
            cleanup();
            // Ignore AbortError that occurs when we intentionally abort the watch
            if (err && !(err instanceof Error && err.name === "AbortError")) {
              reject(err);
            }
          },
        )
        .then((ac) => {
          abortController = ac;
        });
    });
  }

  private watchForSandboxReady(
    sandboxName: string,
    timeoutMs: number,
  ): Promise<void> {
    console.info("Watching for Sandbox to become ready...");

    const watcher = new k8s.Watch(this.kubeConfig);

    return new Promise<void>((resolve, reject) => {
      let abortController: AbortController | undefined;
      let timer: ReturnType<typeof setTimeout>;

      const cleanup = () => {
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
          new Error(
            `Sandbox did not become ready within ${this.sandboxReadyTimeout} seconds.`,
          ),
        );
      }, timeoutMs);

      watcher
        .watch(
          `/apis/${SANDBOX_API_GROUP}/${SANDBOX_API_VERSION}/namespaces/${this.namespace}/${SANDBOX_PLURAL_NAME}`,
          { fieldSelector: `metadata.name=${sandboxName}` },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ADDED" || type === "MODIFIED") {
              const status = (obj.status as Record<string, unknown>) ?? {};
              const conditions =
                (status.conditions as Array<Record<string, string>>) ?? [];
              const isReady = conditions.some(
                (c) => c.type === "Ready" && c.status === "True",
              );

              if (isReady) {
                const metadata =
                  (obj.metadata as Record<string, unknown>) ?? {};
                this.sandboxName = metadata.name as string | undefined;
                if (!this.sandboxName) {
                  cleanup();
                  reject(
                    new Error(
                      "Could not determine sandbox name from sandbox object.",
                    ),
                  );
                  return;
                }
                console.info(`Sandbox ${this.sandboxName} is ready.`);

                this.annotations =
                  (metadata.annotations as Record<string, string>) ?? {};
                const podName = this.annotations[POD_NAME_ANNOTATION];
                if (podName) {
                  this.podName = podName;
                  console.info(
                    `Found pod name from annotation: ${this.podName}`,
                  );
                } else {
                  this.podName = this.sandboxName;
                }

                cleanup();
                resolve();
              }
            }
          },
          (err) => {
            cleanup();
            // Ignore AbortError that occurs when we intentionally abort the watch
            if (err && !(err instanceof Error && err.name === "AbortError")) {
              reject(err);
            }
          },
        )
        .then((ac) => {
          abortController = ac;
        });
    });
  }

  private async waitForSandboxReady(): Promise<void> {
    const fn = async () => {
      if (!this.claimName) {
        throw new Error(
          "Cannot wait for sandbox; a sandboxclaim has not been created.",
        );
      }

      const startTime = Date.now();
      const totalTimeoutMs = this.sandboxReadyTimeout * 1000;

      // Step 1: Watch SandboxClaim until status.sandbox.name is populated.
      // This resolves the actual Sandbox name, which may differ from the claim
      // name when a sandbox is adopted from a warm pool.
      const sandboxName = await this.resolveSandboxName(totalTimeoutMs);

      // Step 2: Watch the Sandbox by its resolved name with the remaining budget.
      const elapsed = Date.now() - startTime;
      const remainingMs = Math.max(0, totalTimeoutMs - elapsed);
      await this.watchForSandboxReady(sandboxName, remainingMs);
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "wait_for_sandbox_ready",
      fn,
      this.tracingManager?.parentContext,
    );
  }

  private async waitForGatewayIp(): Promise<void> {
    const fn = async () => {
      const span = getCurrentSpan();
      if (span.isRecording()) {
        span.setAttribute("sandbox.gateway.name", this.gatewayName!);
        span.setAttribute("sandbox.gateway.namespace", this.gatewayNamespace);
      }

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
        const is404 = err instanceof Error && err.message.includes("404");
        if (!is404) {
          throw err;
        }
      }

      const watcher = new k8s.Watch(this.kubeConfig);
      const timeoutMs = this.gatewayReadyTimeout * 1000;

      await new Promise<void>((resolve, reject) => {
        let abortController: AbortController | undefined;
        let timer: ReturnType<typeof setTimeout>;

        const cleanup = () => {
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
            new Error(
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
              }
            },
            (err) => {
              cleanup();
              // Ignore AbortError that occurs when we intentionally abort the watch
              if (err && !(err instanceof Error && err.name === "AbortError")) {
                reject(err);
              }
            },
          )
          .then((ac) => {
            abortController = ac;
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
        if (this.portForwardProcess.exitCode !== null) {
          throw new Error(
            `Tunnel crashed: port-forward process exited with code ${this.portForwardProcess.exitCode}`,
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
      throw new Error("Failed to establish tunnel to Router Service.");
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
    if (!this.isReady()) {
      throw new Error("Sandbox is not ready for communication.");
    }

    if (this.portForwardProcess && this.portForwardProcess.exitCode !== null) {
      throw new Error(
        `Kubectl Port-Forward crashed BEFORE request! ` +
          `Exit code: ${this.portForwardProcess.exitCode}`,
      );
    }

    const url = `${this.baseUrl!.replace(/\/+$/, "")}/${endpoint.replace(
      /^\/+/,
      "",
    )}`;

    const headers: Record<string, string> = {
      ...options.headers,
      "X-Sandbox-ID": this.sandboxName!,
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
        throw new Error(
          `Request failed with status ${response.status}: ${response.statusText}`,
        );
      }
      return response;
    } catch (err) {
      if (
        this.portForwardProcess &&
        this.portForwardProcess.exitCode !== null
      ) {
        throw new Error(
          `Kubectl Port-Forward crashed DURING request! ` +
            `Exit code: ${this.portForwardProcess.exitCode}`,
          { cause: err },
        );
      }

      console.error(`Request to gateway router failed: ${err}`);
      throw new Error(
        `Failed to communicate with the sandbox via the gateway at ${url}.`,
        { cause: err },
      );
    }
  }
}
