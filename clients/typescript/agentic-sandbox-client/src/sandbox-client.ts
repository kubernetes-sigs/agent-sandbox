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
import * as k8s from "@kubernetes/client-node";

import type { CreateSandboxOptions, SandboxClientOptions } from "./types.js";
import { Sandbox } from "./sandbox.js";
import type { SandboxInit } from "./sandbox.js";
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  POD_NAME_ANNOTATION,
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
import {
  isK8s404,
  SandboxError,
  SandboxMetadataError,
  SandboxNotFoundError,
  SandboxNotReadyError,
  SandboxTimeoutError,
} from "./exceptions.js";

// Kubernetes label validation constraints
// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#syntax-and-character-set
const LABEL_NAME_RE = /^[A-Za-z0-9][-A-Za-z0-9_.]*[A-Za-z0-9]$|^[A-Za-z0-9]$/;
const LABEL_PREFIX_RE = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
const LABEL_NAME_MAX_LENGTH = 63;
const LABEL_PREFIX_MAX_LENGTH = 253;

function validateLabelName(name: string, context: string): void {
  if (name.length > LABEL_NAME_MAX_LENGTH) {
    throw new Error(
      `Label ${context} '${name}' exceeds max length of ${LABEL_NAME_MAX_LENGTH} characters.`,
    );
  }
  if (!LABEL_NAME_RE.test(name)) {
    throw new Error(
      `Label ${context} '${name}' contains invalid characters. ` +
        `Must start and end with alphanumeric, and contain only [-A-Za-z0-9_.].`,
    );
  }
}

function validateLabels(labels: Record<string, string>): void {
  for (const [key, value] of Object.entries(labels)) {
    if (!key) {
      throw new Error("Label key cannot be empty.");
    }

    if (key.includes("/")) {
      const slashIdx = key.indexOf("/");
      const prefix = key.slice(0, slashIdx);
      const name = key.slice(slashIdx + 1);

      if (!prefix || prefix.length > LABEL_PREFIX_MAX_LENGTH) {
        throw new Error(
          `Label key prefix '${prefix}' is invalid or exceeds ${LABEL_PREFIX_MAX_LENGTH} characters.`,
        );
      }
      if (!LABEL_PREFIX_RE.test(prefix)) {
        throw new Error(
          `Label key prefix '${prefix}' must be a valid DNS subdomain.`,
        );
      }
      if (!name) {
        throw new Error(`Label key '${key}' has an empty name after prefix.`);
      }
      validateLabelName(name, `key name in '${key}'`);
    } else {
      validateLabelName(key, `key '${key}'`);
    }

    // Values can be empty; non-empty values must follow the same name constraints
    if (value) {
      validateLabelName(value, `value '${value}' for key '${key}'`);
    }
  }
}

function isValidDNSLabel(s: string): boolean {
  if (s.length === 0 || s.length > 63) return false;
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(s);
}

function isValidDNSSubdomain(s: string): boolean {
  if (s.length === 0 || s.length > 253) return false;
  return (
    /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/.test(s) &&
    !s.includes("..") &&
    !s.includes(".-") &&
    !s.includes("-.")
  );
}

/**
 * Registry-based client for managing multiple Sandbox handles.
 * Tracks all created sandboxes and supports creating, retrieving,
 * listing, and deleting them.
 */
export class SandboxClient<T extends Sandbox = Sandbox> {
  protected readonly sandboxClass: new (
    init: SandboxInit,
  ) => T;

  private readonly defaultNamespace: string;
  private readonly apiUrl: string | undefined;
  private readonly gatewayName: string | undefined;
  private readonly gatewayNamespace: string;
  private readonly serverPort: number;
  private readonly defaultSandboxReadyTimeout: number;
  private readonly gatewayReadyTimeout: number;
  private readonly portForwardReadyTimeout: number;
  private readonly enableTracing: boolean;
  private readonly traceServiceName: string;

  private tracerInitialized = false;
  private autoCleanupActive = false;

  protected readonly kubeConfig: k8s.KubeConfig;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;

  private readonly registry: Map<string, T> = new Map();

  constructor(options: SandboxClientOptions = {}) {
    if (options.serverPort !== undefined) {
      if (
        !Number.isInteger(options.serverPort) ||
        options.serverPort < 1 ||
        options.serverPort > 65535
      ) {
        throw new SandboxError(
          `serverPort must be an integer between 1 and 65535, got: ${options.serverPort}`,
        );
      }
    }
    for (const [key, value] of [
      ["sandboxReadyTimeout", options.sandboxReadyTimeout],
      ["gatewayReadyTimeout", options.gatewayReadyTimeout],
      ["portForwardReadyTimeout", options.portForwardReadyTimeout],
    ] as [string, number | undefined][]) {
      if (value !== undefined && value <= 0) {
        throw new SandboxError(
          `${key} must be a positive number, got: ${value}`,
        );
      }
    }
    for (const [key, value] of [
      ["namespace", options.namespace],
      ["gatewayNamespace", options.gatewayNamespace],
    ] as [string, string | undefined][]) {
      if (value !== undefined && value.length === 0) {
        throw new SandboxError(`${key} must be a non-empty string`);
      }
    }

    // apiUrl URL structure validation (http/https scheme, non-empty host)
    if (options.apiUrl !== undefined) {
      let parsedUrl: URL;
      try {
        parsedUrl = new URL(options.apiUrl);
      } catch {
        throw new SandboxError(
          `apiUrl must be a valid URL, got: ${options.apiUrl}`,
        );
      }
      if (parsedUrl.protocol !== "http:" && parsedUrl.protocol !== "https:") {
        throw new SandboxError(
          `apiUrl must use http or https scheme, got: ${parsedUrl.protocol}`,
        );
      }
      if (!parsedUrl.host) {
        throw new SandboxError(
          `apiUrl must include a host, got: ${options.apiUrl}`,
        );
      }
    }

    // DNS label validation for namespace and gatewayNamespace
    for (const [key, value] of [
      ["namespace", options.namespace],
      ["gatewayNamespace", options.gatewayNamespace],
    ] as [string, string | undefined][]) {
      if (value !== undefined && value.length > 0 && !isValidDNSLabel(value)) {
        throw new SandboxError(
          `${key} must be a valid Kubernetes namespace (DNS label): ` +
            `lowercase alphanumeric or hyphens, max 63 characters, got: ${value}`,
        );
      }
    }

    // DNS subdomain validation for gatewayName
    if (
      options.gatewayName !== undefined &&
      options.gatewayName.length > 0 &&
      !isValidDNSSubdomain(options.gatewayName)
    ) {
      throw new SandboxError(
        `gatewayName must be a valid Kubernetes DNS subdomain name, got: ${options.gatewayName}`,
      );
    }

    this.defaultNamespace = options.namespace ?? "default";
    this.apiUrl = options.apiUrl;
    this.gatewayName = options.gatewayName;
    this.gatewayNamespace = options.gatewayNamespace ?? "default";
    this.serverPort = options.serverPort ?? 8888;
    this.defaultSandboxReadyTimeout = options.sandboxReadyTimeout ?? 180;
    this.gatewayReadyTimeout = options.gatewayReadyTimeout ?? 180;
    this.portForwardReadyTimeout = options.portForwardReadyTimeout ?? 30;
    this.enableTracing = options.enableTracing ?? false;
    this.traceServiceName = options.traceServiceName ?? "sandbox-client";

    this.kubeConfig = new k8s.KubeConfig();
    this.kubeConfig.loadFromDefault();

    const clusters = this.kubeConfig.clusters ?? [];
    const isOnlyFallback =
      clusters.length === 0 ||
      clusters.every((c) => c.server === "http://localhost:8080");
    if (isOnlyFallback) {
      throw new SandboxError(
        "No Kubernetes configuration found. " +
          "Set KUBECONFIG, provide ~/.kube/config, or run inside a cluster.",
      );
    }
    this.customObjectsApi = this.kubeConfig.makeApiClient(k8s.CustomObjectsApi);

    this.sandboxClass = Sandbox as unknown as new (init: SandboxInit) => T;
  }

  /**
   * Provisions a new Sandbox and returns a managed handle.
   * On failure, any orphaned SandboxClaim is cleaned up automatically.
   */
  async createSandbox(
    template: string,
    namespace?: string,
    opts?: CreateSandboxOptions,
  ): Promise<T> {
    if (!template) {
      throw new Error("Template name cannot be empty.");
    }

    // Review #16: normalize empty string to defaultNamespace (matches Go behaviour)
    const ns = namespace || this.defaultNamespace;
    const sandboxReadyTimeout =
      opts?.sandboxReadyTimeout ?? this.defaultSandboxReadyTimeout;

    await this.ensureTracer();

    // Create the per-sandbox tracer manager BEFORE createClaim so that
    // createClaim and waitForSandboxReady run as children of the lifecycle span.
    let sandboxTracingManager: TracerManager | null = null;
    let sandboxTracer: Tracer | null = null;
    if (this.enableTracing) {
      sandboxTracingManager = new TracerManager(this.traceServiceName);
      sandboxTracer = sandboxTracingManager.tracer;
      sandboxTracingManager.startLifecycleSpan();
    }

    const claimName = `sandbox-claim-${crypto.randomBytes(4).toString("hex")}`;

    let sandboxName: string;
    let podName: string;
    let annotations: Record<string, string>;

    try {
      const traceContextStr =
        sandboxTracingManager?.getTraceContextJson() ?? "";
      await this.createClaim(
        claimName,
        template,
        ns,
        opts?.labels,
        traceContextStr,
        sandboxTracer,
        sandboxTracingManager?.parentContext,
      );
      ({ sandboxName, podName, annotations } = await this.waitForSandboxReady(
        claimName,
        ns,
        sandboxReadyTimeout * 1000,
        sandboxTracer,
        sandboxTracingManager?.parentContext,
      ));
    } catch (err) {
      sandboxTracingManager?.endLifecycleSpan();
      // Clean up orphaned claim before re-throwing
      try {
        await this.customObjectsApi.deleteNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: ns,
          plural: CLAIM_PLURAL_NAME,
          name: claimName,
        });
      } catch {
        // ignore cleanup errors
      }
      throw err;
    }

    const init: SandboxInit = {
      claimName,
      sandboxName,
      podName,
      namespace: ns,
      annotations,
      serverPort: this.serverPort,
      apiUrl: this.apiUrl,
      gatewayName: this.gatewayName,
      gatewayNamespace: this.gatewayNamespace,
      gatewayReadyTimeout: this.gatewayReadyTimeout,
      portForwardReadyTimeout: this.portForwardReadyTimeout,
      kubeConfig: this.kubeConfig,
      customObjectsApi: this.customObjectsApi,
      traceServiceName: this.traceServiceName,
      tracer: sandboxTracer,
      tracingManager: sandboxTracingManager,
    };

    const sandbox = new this.sandboxClass(init);

    try {
      await sandbox.connect();
    } catch (err) {
      // connect() failed — close sandbox (which also deletes claim)
      await sandbox.close().catch(() => {});
      throw err;
    }

    this.registry.set(`${ns}/${claimName}`, sandbox);
    return sandbox;
  }

  /**
   * Retrieves an existing sandbox handle by claim name.
   * Returns the cached handle if still active, otherwise re-attaches.
   */
  async getSandbox(claimName: string, namespace?: string): Promise<T> {
    // normalize empty string to defaultNamespace (matches Go behaviour)
    const ns = namespace || this.defaultNamespace;
    const key = `${ns}/${claimName}`;

    const existing = this.registry.get(key);
    if (existing?.isActive) {
      try {
        await this.customObjectsApi.getNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: ns,
          plural: CLAIM_PLURAL_NAME,
          name: claimName,
        });
        return existing; // claim still exists — safe to return cached handle
      } catch (err) {
        // Claim gone (404) or unreachable — evict and surface the error immediately
        this.registry.delete(key);
        throw new SandboxNotFoundError(
          `SandboxClaim '${claimName}' not found in namespace '${ns}'.`,
          { cause: err },
        );
      }
    }

    // Evict stale handle
    if (existing) {
      this.registry.delete(key);
    }

    // Verify the claim exists in Kubernetes
    try {
      await this.customObjectsApi.getNamespacedCustomObject({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace: ns,
        plural: CLAIM_PLURAL_NAME,
        name: claimName,
      });
    } catch (err) {
      throw new SandboxNotFoundError(
        `SandboxClaim '${claimName}' not found in namespace '${ns}'.`,
        { cause: err },
      );
    }

    await this.ensureTracer();

    let sandboxTracingManager: TracerManager | null = null;
    let sandboxTracer: Tracer | null = null;
    if (this.enableTracing) {
      sandboxTracingManager = new TracerManager(this.traceServiceName);
      sandboxTracer = sandboxTracingManager.tracer;
      sandboxTracingManager.startLifecycleSpan();
    }

    // Resolve the sandbox identity and wait for readiness
    let sandboxName: string;
    let podName: string;
    let annotations: Record<string, string>;
    try {
      ({ sandboxName, podName, annotations } = await this.waitForSandboxReady(
        claimName,
        ns,
        this.defaultSandboxReadyTimeout * 1000,
        sandboxTracer,
        sandboxTracingManager?.parentContext,
      ));
    } catch (err) {
      sandboxTracingManager?.endLifecycleSpan();
      throw err;
    }

    const init: SandboxInit = {
      claimName,
      sandboxName,
      podName,
      namespace: ns,
      annotations,
      serverPort: this.serverPort,
      apiUrl: this.apiUrl,
      gatewayName: this.gatewayName,
      gatewayNamespace: this.gatewayNamespace,
      gatewayReadyTimeout: this.gatewayReadyTimeout,
      portForwardReadyTimeout: this.portForwardReadyTimeout,
      kubeConfig: this.kubeConfig,
      customObjectsApi: this.customObjectsApi,
      traceServiceName: this.traceServiceName,
      tracer: sandboxTracer,
      tracingManager: sandboxTracingManager,
    };

    const sandbox = new this.sandboxClass(init);
    try {
      await sandbox.connect();
    } catch (err) {
      // Release local resources (port-forward process, tracing span) on
      // connect failure, but do NOT delete the SandboxClaim — the claim belongs
      // to the existing sandbox which may still be healthy; only the local tunnel
      // setup failed.
      await sandbox.closeLocal().catch(() => {});
      throw err;
    }

    this.registry.set(key, sandbox);
    return sandbox;
  }

  /**
   * Returns keys of sandboxes currently tracked and still active.
   * Prunes inactive handles from the registry.
   */
  listActiveSandboxes(): Array<{ namespace: string; claimName: string }> {
    const active: Array<{ namespace: string; claimName: string }> = [];
    for (const [key, sandbox] of this.registry) {
      if (!sandbox.isActive) {
        this.registry.delete(key);
        continue;
      }
      const slashIdx = key.indexOf("/");
      active.push({
        namespace: key.slice(0, slashIdx),
        claimName: key.slice(slashIdx + 1),
      });
    }
    return active;
  }

  /**
   * Lists all SandboxClaim names in the cluster for the given namespace.
   */
  async listAllSandboxes(namespace?: string): Promise<string[]> {
    const ns = namespace ?? this.defaultNamespace;
    const response = await this.customObjectsApi.listNamespacedCustomObject({
      group: CLAIM_API_GROUP,
      version: CLAIM_API_VERSION,
      namespace: ns,
      plural: CLAIM_PLURAL_NAME,
    });
    const list = response as {
      items?: Array<{ metadata?: { name?: string } }>;
    };
    return (list.items ?? [])
      .map((item) => item.metadata?.name ?? "")
      .filter(Boolean);
  }

  /**
   * Closes the sandbox handle (if tracked) and deletes the Kubernetes resources.
   */
  async deleteSandbox(claimName: string, namespace?: string): Promise<void> {
    const ns = namespace ?? this.defaultNamespace;
    const key = `${ns}/${claimName}`;

    const sandbox = this.registry.get(key);
    this.registry.delete(key);

    if (sandbox) {
      await sandbox.close();
    } else {
      // Not tracked locally; delete the claim directly
      try {
        await this.customObjectsApi.deleteNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: ns,
          plural: CLAIM_PLURAL_NAME,
          name: claimName,
        });
      } catch (err: unknown) {
        if (!isK8s404(err)) {
          throw err;
        }
      }
    }
  }

  /**
   * Closes and deletes all tracked sandboxes. Best-effort.
   */
  async deleteAll(): Promise<void> {
    const snapshot = new Map(this.registry);
    this.registry.clear();

    const results = await Promise.allSettled(
      [...snapshot.values()].map((sandbox) => sandbox.close()),
    );

    for (const result of results) {
      if (result.status === "rejected") {
        console.error(`Cleanup failed: ${result.reason}`);
      }
    }
  }

  /**
   * Registers SIGINT, SIGTERM, and beforeExit handlers to call deleteAll().
   * Returns a function that unregisters the handlers.
   * Idempotent: subsequent calls return a no-op until the returned stop function is called.
   */
  enableAutoCleanup(): () => void {
    if (this.autoCleanupActive) {
      return () => {};
    }
    this.autoCleanupActive = true;

    const beforeExitHandler = () => {
      void this.deleteAll();
    };

    const sigHandler = (signal: NodeJS.Signals) => {
      process.off("beforeExit", beforeExitHandler);
      process.off("SIGINT", sigHandler);
      process.off("SIGTERM", sigHandler);
      this.autoCleanupActive = false;
      void this.deleteAll().finally(() => {
        process.kill(process.pid, signal);
      });
    };

    process.on("beforeExit", beforeExitHandler);
    process.on("SIGINT", sigHandler);
    process.on("SIGTERM", sigHandler);

    return () => {
      process.off("beforeExit", beforeExitHandler);
      process.off("SIGINT", sigHandler);
      process.off("SIGTERM", sigHandler);
      this.autoCleanupActive = false;
    };
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.deleteAll();
  }

  // -------------------------------------------------------------------------
  // Private: Kubernetes provisioning helpers
  // -------------------------------------------------------------------------

  private async ensureTracer(): Promise<void> {
    if (this.tracerInitialized || !this.enableTracing) return;
    await initializeTracer(this.traceServiceName);
    this.tracerInitialized = true;
  }

  private async createClaim(
    claimName: string,
    template: string,
    namespace: string,
    labels?: Record<string, string>,
    traceContextStr: string = "",
    tracer: Tracer | null = null,
    parentContext?: unknown,
  ): Promise<void> {
    if (labels) {
      validateLabels(labels);
    }

    const fn = async () => {
      const span = getCurrentSpan();
      if (span.isRecording()) {
        span.setAttribute("sandbox.claim.name", claimName);
      }

      const annotations: Record<string, string> = {};
      if (traceContextStr) {
        annotations["opentelemetry.io/trace-context"] = traceContextStr;
      }

      const manifest: Record<string, unknown> = {
        apiVersion: `${CLAIM_API_GROUP}/${CLAIM_API_VERSION}`,
        kind: "SandboxClaim",
        metadata: {
          name: claimName,
          namespace,
          annotations,
          ...(labels ? { labels } : {}),
        },
        spec: {
          sandboxTemplateRef: { name: template },
        },
      };

      console.info(
        `Creating SandboxClaim '${claimName}' ` +
          `in namespace '${namespace}' ` +
          `using template '${template}'...`,
      );

      await this.customObjectsApi.createNamespacedCustomObject({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace,
        plural: CLAIM_PLURAL_NAME,
        body: manifest,
      });
    };

    await withSpan(
      tracer,
      this.traceServiceName,
      "create_claim",
      fn,
      parentContext,
    );
  }

  private async resolveSandboxName(
    claimName: string,
    namespace: string,
    timeoutMs: number,
  ): Promise<string> {
    console.info(`Resolving sandbox name from claim '${claimName}'...`);

    // Initial GET: check if claim is already resolved before starting a watch.
    // Matches Go/Python pattern: list/get first so we don't miss resources that
    // were already resolved before the watch stream is established.
    try {
      const existing = await this.customObjectsApi.getNamespacedCustomObject({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace,
        plural: CLAIM_PLURAL_NAME,
        name: claimName,
      });
      const status =
        ((existing as Record<string, unknown>)?.status as Record<
          string,
          unknown
        >) ?? {};
      const sandboxStatus = (status.sandbox as Record<string, unknown>) ?? {};
      const name = sandboxStatus.name as string | undefined;
      if (name) {
        console.info(
          `Resolved sandbox name '${name}' from claim status (initial GET).`,
        );
        return name;
      }
    } catch {
      // Claim may not exist yet or sandbox name not set — proceed to watch
    }

    const watcher = new k8s.Watch(this.kubeConfig);

    return new Promise<string>((resolve, reject) => {
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
          new SandboxTimeoutError(
            `Sandbox claim '${claimName}' did not resolve within ${Math.floor(timeoutMs / 1000)} seconds.`,
          ),
        );
      }, timeoutMs);

      watcher
        .watch(
          `/apis/${CLAIM_API_GROUP}/${CLAIM_API_VERSION}/namespaces/${namespace}/${CLAIM_PLURAL_NAME}`,
          { fieldSelector: `metadata.name=${claimName}` },
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
            } else if (type === "DELETED") {
              cleanup();
              reject(
                new SandboxMetadataError(
                  `SandboxClaim '${claimName}' was deleted while waiting for it to be resolved.`,
                ),
              );
            }
          },
          (err) => {
            const wasAborted = aborted;
            cleanup();
            if (!wasAborted) {
              if (err && !(err instanceof Error && err.name === "AbortError")) {
                reject(err);
              } else if (!err) {
                // done(null): watch ended cleanly without resolving — reject to avoid hanging
                reject(
                  new SandboxNotReadyError(
                    `Watch for claim '${claimName}' ended without resolving.`,
                  ),
                );
              }
            }
          },
        )
        .then((ac) => {
          if (aborted) {
            ac.abort();
          } else {
            abortController = ac;
          }
        })
        .catch((err: unknown) => {
          // watcher.watch() itself rejected (auth error, network down, etc.)
          if (!aborted) {
            cleanup();
            reject(err instanceof Error ? err : new Error(String(err)));
          }
        });
    });
  }

  private async watchForSandboxReady(
    sandboxName: string,
    namespace: string,
    timeoutMs: number,
  ): Promise<{ podName: string; annotations: Record<string, string> }> {
    console.info("Watching for Sandbox to become ready...");

    // Initial GET: check if sandbox is already Ready before starting a watch.
    // Matches Go/Python: get/list first to avoid missing already-ready resources.
    try {
      const existing = await this.customObjectsApi.getNamespacedCustomObject({
        group: SANDBOX_API_GROUP,
        version: SANDBOX_API_VERSION,
        namespace,
        plural: SANDBOX_PLURAL_NAME,
        name: sandboxName,
      });
      const obj = existing as Record<string, unknown>;
      const status = (obj?.status as Record<string, unknown>) ?? {};
      const conditions =
        (status.conditions as Array<Record<string, string>>) ?? [];
      const isReady = conditions.some(
        (c) => c.type === "Ready" && c.status === "True",
      );
      if (isReady) {
        const metadata = (obj?.metadata as Record<string, unknown>) ?? {};
        const resolvedName = metadata.name as string | undefined;
        if (resolvedName) {
          console.info(
            `Sandbox ${resolvedName} is already ready (initial GET).`,
          );
          const annotations =
            (metadata.annotations as Record<string, string>) ?? {};
          const podNameAnnotation = annotations[POD_NAME_ANNOTATION];
          const podName = podNameAnnotation ?? resolvedName;
          if (podNameAnnotation) {
            console.info(`Found pod name from annotation: ${podName}`);
          }
          return { podName, annotations };
        }
      }
    } catch {
      // Sandbox may not exist yet or not ready — proceed to watch
    }

    const watcher = new k8s.Watch(this.kubeConfig);

    return new Promise((resolve, reject) => {
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
          new SandboxTimeoutError(
            `Sandbox '${sandboxName}' did not become ready within ${Math.floor(timeoutMs / 1000)} seconds.`,
          ),
        );
      }, timeoutMs);

      watcher
        .watch(
          `/apis/${SANDBOX_API_GROUP}/${SANDBOX_API_VERSION}/namespaces/${namespace}/${SANDBOX_PLURAL_NAME}`,
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
                const resolvedName = metadata.name as string | undefined;
                if (!resolvedName) {
                  cleanup();
                  reject(
                    new SandboxMetadataError(
                      "Could not determine sandbox name from sandbox object.",
                    ),
                  );
                  return;
                }
                console.info(`Sandbox ${resolvedName} is ready.`);

                const annotations =
                  (metadata.annotations as Record<string, string>) ?? {};
                const podNameAnnotation = annotations[POD_NAME_ANNOTATION];
                const podName = podNameAnnotation ?? resolvedName;
                if (podNameAnnotation) {
                  console.info(`Found pod name from annotation: ${podName}`);
                }

                cleanup();
                resolve({ podName, annotations });
              }
            } else if (type === "DELETED") {
              cleanup();
              reject(
                new SandboxNotFoundError(
                  `Sandbox '${sandboxName}' was deleted while waiting for it to become ready.`,
                ),
              );
            }
          },
          (err) => {
            const wasAborted = aborted;
            cleanup();
            if (!wasAborted) {
              if (err && !(err instanceof Error && err.name === "AbortError")) {
                reject(err);
              } else if (!err) {
                // done(null): watch ended cleanly without sandbox becoming ready
                reject(
                  new SandboxNotReadyError(
                    `Watch for sandbox '${sandboxName}' ended without it becoming ready.`,
                  ),
                );
              }
            }
          },
        )
        .then((ac) => {
          if (aborted) {
            ac.abort();
          } else {
            abortController = ac;
          }
        })
        .catch((err: unknown) => {
          // watcher.watch() itself rejected (auth error, network down, etc.)
          if (!aborted) {
            cleanup();
            reject(err instanceof Error ? err : new Error(String(err)));
          }
        });
    });
  }

  private async waitForSandboxReady(
    claimName: string,
    namespace: string,
    totalTimeoutMs: number,
    tracer: Tracer | null = null,
    parentContext?: unknown,
  ): Promise<{
    sandboxName: string;
    podName: string;
    annotations: Record<string, string>;
  }> {
    const fn = async () => {
      const startTime = Date.now();

      // Step 1: Resolve actual sandbox name from claim status
      const sandboxName = await this.resolveSandboxName(
        claimName,
        namespace,
        totalTimeoutMs,
      );

      // Step 2: Watch sandbox with remaining budget
      const elapsed = Date.now() - startTime;
      const remainingMs = Math.max(0, totalTimeoutMs - elapsed);
      const { podName, annotations } = await this.watchForSandboxReady(
        sandboxName,
        namespace,
        remainingMs,
      );

      return { sandboxName, podName, annotations };
    };

    return withSpan(
      tracer,
      this.traceServiceName,
      "wait_for_sandbox_ready",
      fn,
      parentContext,
    );
  }
}
