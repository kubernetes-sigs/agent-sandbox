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
import * as path from "node:path";
import { ChildProcess, spawn } from "node:child_process";
import * as k8s from "@kubernetes/client-node";

import type { ExecutionResult, SandboxOptions } from "./types.js";
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
      if (
        attempt < maxRetries &&
        retryStatusCodes.includes(response.status)
      ) {
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
    this.customObjectsApi = this.kubeConfig.makeApiClient(
      k8s.CustomObjectsApi,
    );
  }

  isReady(): boolean {
    return this.baseUrl !== undefined;
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

    await withSpan(this.tracer, this.traceServiceName, "create_claim", fn);
  }

  private async waitForSandboxReady(): Promise<void> {
    const fn = async () => {
      if (!this.claimName) {
        throw new Error(
          "Cannot wait for sandbox; a sandboxclaim has not been created.",
        );
      }

      console.info("Watching for Sandbox to become ready...");

      const watcher = new k8s.Watch(this.kubeConfig);
      const timeoutMs = this.sandboxReadyTimeout * 1000;

      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          reject(
            new Error(
              `Sandbox did not become ready within ${this.sandboxReadyTimeout} seconds.`,
            ),
          );
        }, timeoutMs);

        let abortController: AbortController | undefined;

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

        watcher.watch(
          `/apis/${SANDBOX_API_GROUP}/${SANDBOX_API_VERSION}/namespaces/${this.namespace}/${SANDBOX_PLURAL_NAME}`,
          { fieldSelector: `metadata.name=${this.claimName}` },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ADDED" || type === "MODIFIED") {
              const status = (obj.status as Record<string, unknown>) ?? {};
              const conditions =
                (status.conditions as Array<Record<string, string>>) ?? [];
              const isReady = conditions.some(
                (c) => c.type === "Ready" && c.status === "True",
              );

              if (isReady) {
                const metadata = (obj.metadata as Record<string, unknown>) ??
                  {};
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
        ).then((ac) => {
          abortController = ac;
        });
      });
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "wait_for_sandbox_ready",
      fn,
    );
  }

  private async waitForGatewayIp(): Promise<void> {
    const fn = async () => {
      const span = getCurrentSpan();
      if (span.isRecording()) {
        span.setAttribute("sandbox.gateway.name", this.gatewayName!);
        span.setAttribute(
          "sandbox.gateway.namespace",
          this.gatewayNamespace,
        );
      }

      if (this.baseUrl) {
        console.info(`Using configured API URL: ${this.baseUrl}`);
        return;
      }

      console.info(
        `Waiting for Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}'...`,
      );

      const watcher = new k8s.Watch(this.kubeConfig);
      const timeoutMs = this.gatewayReadyTimeout * 1000;

      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          reject(
            new Error(
              `Gateway '${this.gatewayName}' in namespace '${this.gatewayNamespace}' did not get ` +
                `an IP within ${this.gatewayReadyTimeout} seconds.`,
            ),
          );
        }, timeoutMs);

        let abortController: AbortController | undefined;

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

        watcher.watch(
          `/apis/${GATEWAY_API_GROUP}/${GATEWAY_API_VERSION}/namespaces/${this.gatewayNamespace}/${GATEWAY_PLURAL}`,
          { fieldSelector: `metadata.name=${this.gatewayName}` },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ADDED" || type === "MODIFIED") {
              const status = (obj.status as Record<string, unknown>) ?? {};
              const addresses =
                (status.addresses as Array<Record<string, string>>) ?? [];
              if (addresses.length > 0) {
                const ipAddress = addresses[0].value;
                if (ipAddress) {
                  this.baseUrl = `http://${ipAddress}`;
                  console.info(
                    `Gateway is ready. Base URL set to: ${this.baseUrl}`,
                  );
                  cleanup();
                  resolve();
                }
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
        ).then((ac) => {
          abortController = ac;
        });
      });
    };

    await withSpan(
      this.tracer,
      this.traceServiceName,
      "wait_for_gateway",
      fn,
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
        [
          "port-forward",
          routerSvc,
          `${localPort}:8080`,
          "-n",
          this.namespace,
        ],
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
          console.info(
            `Dev Mode ready. Tunneled to Router at ${this.baseUrl}`,
          );
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

    if (
      this.portForwardProcess &&
      this.portForwardProcess.exitCode !== null
    ) {
      throw new Error(
        `Kubectl Port-Forward crashed BEFORE request! ` +
          `Exit code: ${this.portForwardProcess.exitCode}`,
      );
    }

    const url = `${this.baseUrl!.replace(/\/+$/, "")}/${
      endpoint.replace(/^\/+/, "")
    }`;

    const headers: Record<string, string> = {
      ...options.headers,
      "X-Sandbox-ID": this.claimName!,
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
        );
      }

      console.error(`Request to gateway router failed: ${err}`);
      throw new Error(
        `Failed to communicate with the sandbox via the gateway at ${url}.`,
      );
    }
  }

  async run(
    command: string,
    timeout: number = 60,
  ): Promise<ExecutionResult> {
    return withSpan(
      this.tracer,
      this.traceServiceName,
      "run",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.command", command);
        }

        const response = await this.request("POST", "execute", {
          body: JSON.stringify({ command }),
          headers: { "Content-Type": "application/json" },
          timeout,
        });

        const data = (await response.json()) as Record<string, unknown>;
        const result: ExecutionResult = {
          stdout: (data.stdout as string) ?? "",
          stderr: (data.stderr as string) ?? "",
          exitCode: (data.exit_code as number) ?? -1,
        };

        if (span.isRecording()) {
          span.setAttribute("sandbox.exit_code", result.exitCode);
        }

        return result;
      },
    );
  }

  async write(
    filePath: string,
    content: Buffer | string,
    timeout: number = 60,
  ): Promise<void> {
    await withSpan(
      this.tracer,
      this.traceServiceName,
      "write",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
          span.setAttribute("sandbox.file.size", content.length);
        }

        const contentBytes: Uint8Array<ArrayBuffer> =
          typeof content === "string"
            ? new TextEncoder().encode(content)
            : new Uint8Array(
              content.buffer as ArrayBuffer,
              content.byteOffset,
              content.byteLength,
            );

        const filename = path.basename(filePath);
        const blob = new Blob([contentBytes]);
        const formData = new FormData();
        formData.append("file", blob, filename);

        await this.request("POST", "upload", {
          body: formData,
          timeout,
        });

        console.info(`File '${filename}' uploaded successfully.`);
      },
    );
  }

  async read(filePath: string, timeout: number = 60): Promise<Buffer> {
    return withSpan(
      this.tracer,
      this.traceServiceName,
      "read",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
        }

        const response = await this.request("GET", `download/${filePath}`, {
          timeout,
        });

        const arrayBuffer = await response.arrayBuffer();
        const buffer = Buffer.from(arrayBuffer);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.size", buffer.length);
        }

        return buffer;
      },
    );
  }
}
