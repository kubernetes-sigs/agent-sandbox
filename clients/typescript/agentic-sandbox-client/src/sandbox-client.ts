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
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  CLEANUP_TIMEOUT_MS,
  POD_NAME_ANNOTATION,
  SANDBOX_API_GROUP,
  SANDBOX_API_VERSION,
  SANDBOX_PLURAL_NAME,
  TERMINAL_CLAIM_READY_REASONS,
} from "./constants.js";
import {
  isK8s404,
  isK8s409,
  SandboxClaimFailedError,
  SandboxError,
  SandboxMetadataError,
  SandboxNotFoundError,
  SandboxTemplateNotFoundError,
  SandboxTimeoutError,
  SandboxWarmPoolNotFoundError,
} from "./exceptions.js";
import { resolveLogger } from "./logger.js";
import type { SandboxInit } from "./sandbox.js";
import { raceWithTimeout, Sandbox } from "./sandbox.js";
import type { Tracer } from "./trace-manager.js";
import {
  getCurrentSpan,
  initializeTracer,
  TracerManager,
  withSpan,
} from "./trace-manager.js";
import type {
  CreateSandboxOptions,
  Logger,
  SandboxClientOptions,
} from "./types.js";

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
      if (prefix.includes("..")) {
        throw new Error(
          `Label key prefix '${prefix}' must be a valid DNS subdomain.`,
        );
      }
      if (prefix.split(".").some((seg) => seg.length > 63)) {
        throw new Error(
          `Label key prefix '${prefix}' has a DNS label segment exceeding 63 characters.`,
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

/**
 * Inspects SandboxClaim status conditions and throws a typed error when the
 * controller has signalled a failure it will not recover from on its own
 * (TemplateNotFound, WarmPoolNotFound, or any TERMINAL_CLAIM_READY_REASONS
 * reason). Waiting out the timeout on any of these cannot succeed.
 */
function inspectClaimConditions(
  conditions: Array<Record<string, string>>,
): void {
  for (const cond of conditions) {
    if (cond.type !== "Ready" || cond.status !== "False") {
      continue;
    }
    if (cond.reason === "TemplateNotFound") {
      throw new SandboxTemplateNotFoundError(
        `SandboxTemplate requested does not exist: ${cond.message ?? "Template not found"}`,
      );
    }
    if (cond.reason === "WarmPoolNotFound") {
      throw new SandboxWarmPoolNotFoundError(
        `SandboxWarmPool requested does not exist: ${cond.message ?? "WarmPool not found"}`,
      );
    }
    if (cond.reason && TERMINAL_CLAIM_READY_REASONS.has(cond.reason)) {
      throw new SandboxClaimFailedError(
        `SandboxClaim failed with terminal reason ${cond.reason}: ${cond.message ?? ""}`,
      );
    }
  }
}

function isValidDNSLabel(s: string): boolean {
  if (s.length === 0 || s.length > 63) return false;
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(s);
}

/**
 * Result of a single watch pass.
 * "closed" means the watch stream ended cleanly (done(null)) — caller should re-list.
 */
type WatchPassResult<V> =
  | { type: "resolved"; value: V }
  | { type: "error"; error: Error }
  | { type: "closed" };

/**
 * True when a Watch `done(err)` signals the stream ended without a real
 * failure, so the caller should re-GET and restart the watch from a fresh
 * resourceVersion rather than surface the error:
 *  - `done(null)`: the server closed the long-poll connection normally;
 *  - AbortError: our own AbortController firing (deadline or success);
 *  - TimeoutError: @kubernetes/client-node 2.x aborts every watch after its
 *    30s request timeout — far shorter than the public ready-wait budget;
 *  - 410 Gone: the supplied resourceVersion has been compacted away.
 */
function isWatchCleanClose(err: unknown): boolean {
  if (!err) return true;
  if (
    err instanceof Error &&
    (err.name === "AbortError" || err.name === "TimeoutError")
  ) {
    return true;
  }
  const code =
    (err as { statusCode?: number; code?: number }).statusCode ??
    (err as { statusCode?: number; code?: number }).code;
  return code === 410;
}

/**
 * Registry-based client for managing multiple Sandbox handles.
 * Tracks all created sandboxes and supports creating, retrieving,
 * listing, and deleting them.
 */
export class SandboxClient {
  private readonly defaultNamespace: string;
  private readonly defaultSandboxReadyTimeout: number;
  private readonly enableTracing: boolean;
  private readonly traceServiceName: string;
  private readonly logger: Logger;

  private tracerInitialized = false;
  private autoCleanupActive = false;

  protected readonly kubeConfig: k8s.KubeConfig;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;

  private readonly registry: Map<string, Sandbox> = new Map();

  /**
   * In-flight handle constructions, keyed like the registry: both
   * getSandbox() attaches and createSandbox() provisions publish here so the
   * two can never build competing handles for the same claim.
   */
  private readonly attaching: Map<string, Promise<Sandbox>> = new Map();

  /**
   * Registry keys of createSandbox() calls that have not registered a handle
   * yet. These own a claim that either exists in the cluster or is about to,
   * with nothing in {@link registry} pointing at it, so deleteAll() has to
   * sweep them explicitly or the claim outlives the process. Attaches from
   * getSandbox() are deliberately excluded: their claim predates this client.
   */
  private readonly provisioning: Set<string> = new Set();

  /**
   * Provisions that deleteAll() has swept. provisionSandbox() checks this the
   * moment its claim exists, so a sweep that raced the create POST (its delete
   * 404s, then the POST lands) still tears the claim back down.
   */
  private readonly cancelledProvisions: Set<string> = new Set();

  constructor(options: SandboxClientOptions = {}) {
    if (
      options.sandboxReadyTimeout !== undefined &&
      (!Number.isFinite(options.sandboxReadyTimeout) ||
        options.sandboxReadyTimeout <= 0)
    ) {
      throw new SandboxError(
        `sandboxReadyTimeout must be a positive number, got: ${options.sandboxReadyTimeout}`,
      );
    }
    if (options.namespace !== undefined && options.namespace.length === 0) {
      throw new SandboxError("namespace must be a non-empty string");
    }
    if (
      options.namespace !== undefined &&
      options.namespace.length > 0 &&
      !isValidDNSLabel(options.namespace)
    ) {
      throw new SandboxError(
        "namespace must be a valid Kubernetes namespace (DNS label): " +
          `lowercase alphanumeric or hyphens, max 63 characters, got: ${options.namespace}`,
      );
    }

    this.defaultNamespace = options.namespace ?? "default";
    this.defaultSandboxReadyTimeout = options.sandboxReadyTimeout ?? 180;
    this.enableTracing = options.enableTracing ?? false;
    this.traceServiceName = options.traceServiceName ?? "sandbox-client";
    this.logger = resolveLogger(options.logger, options.quiet);

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
  }

  /**
   * Provisions a new Sandbox and returns a managed handle.
   * On failure, any orphaned SandboxClaim is cleaned up automatically.
   */
  async createSandbox(
    warmpool: string,
    namespace?: string,
    opts?: CreateSandboxOptions,
  ): Promise<Sandbox> {
    if (!warmpool) {
      throw new Error("Warmpool name cannot be empty.");
    }

    // Validate the per-call override with the same rule as the constructor
    // applies to the client default. A non-finite value (NaN, Infinity) would
    // otherwise slip through `??` and defeat every deadline check in the
    // ready-wait loop (`NaN <= 0` is false), hanging the caller.
    if (
      opts?.sandboxReadyTimeout !== undefined &&
      (!Number.isFinite(opts.sandboxReadyTimeout) ||
        opts.sandboxReadyTimeout <= 0)
    ) {
      throw new SandboxError(
        `sandboxReadyTimeout must be a positive number, got: ${opts.sandboxReadyTimeout}`,
      );
    }

    // Empty string normalizes to defaultNamespace (matches Go client behaviour).
    const ns = namespace || this.defaultNamespace;
    const claimName = `sandbox-claim-${crypto.randomBytes(4).toString("hex")}`;
    const key = `${ns}/${claimName}`;

    // Publish the provision under its registry key *before* the claim exists in
    // the cluster, so a concurrent getSandbox() that discovers the claim by
    // listing joins this promise instead of attaching a second handle to it.
    // The registry holds one handle per key, so the loser of that race would
    // otherwise stay active but untracked: deleteAll() would never close it and
    // its lifecycle span would never end. A joined caller shares this call's
    // outcome, including its options and any failure.
    const provision = this.provisionSandbox(
      claimName,
      warmpool,
      ns,
      opts,
    ).finally(() => {
      this.attaching.delete(key);
      this.provisioning.delete(key);
      this.cancelledProvisions.delete(key);
    });
    this.attaching.set(key, provision);
    this.provisioning.add(key);
    return provision;
  }

  /**
   * Creates the SandboxClaim, waits for readiness, and registers the handle.
   * The caller owns the {@link attaching} entry for this claim.
   */
  private async provisionSandbox(
    claimName: string,
    warmpool: string,
    ns: string,
    opts?: CreateSandboxOptions,
  ): Promise<Sandbox> {
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

    let sandboxName: string;
    let podName: string;

    try {
      const traceContextStr =
        sandboxTracingManager?.getTraceContextJson() ?? "";
      await this.createClaim(
        claimName,
        warmpool,
        ns,
        opts?.labels,
        traceContextStr,
        sandboxTracer,
        sandboxTracingManager?.parentContext,
      );
      // deleteAll() may have swept this key while the claim was being created;
      // it could not delete a claim the apiserver had not accepted yet, so fail
      // here and let the rollback below remove it.
      if (this.cancelledProvisions.delete(`${ns}/${claimName}`)) {
        throw new SandboxError(
          `SandboxClaim '${claimName}' was cleaned up while it was being created.`,
        );
      }
      ({ sandboxName, podName } = await this.waitForSandboxReady(
        claimName,
        ns,
        sandboxReadyTimeout * 1000,
        sandboxTracer,
        sandboxTracingManager?.parentContext,
      ));
    } catch (err) {
      sandboxTracingManager?.endLifecycleSpan();
      // Clean up orphaned claim before re-throwing. A 409 means the name is
      // already taken by a claim we never created, so deleting it would take
      // out someone else's Sandbox — skip cleanup in that case.
      if (isK8s409(err)) {
        throw err;
      }
      try {
        await raceWithTimeout(
          this.customObjectsApi.deleteNamespacedCustomObject({
            group: CLAIM_API_GROUP,
            version: CLAIM_API_VERSION,
            namespace: ns,
            plural: CLAIM_PLURAL_NAME,
            name: claimName,
          }),
          CLEANUP_TIMEOUT_MS,
          () => {
            throw new Error("Rollback cleanup timed out");
          },
        );
      } catch (cleanupErr) {
        // A 404 means the claim is already gone — deleteAll() swept it, or it
        // never reached the apiserver. Nothing to roll back, and nothing worth
        // reporting.
        if (!isK8s404(cleanupErr)) {
          this.logger.error(
            `Failed to delete orphaned SandboxClaim '${claimName}': ${cleanupErr}`,
          );
        }
        // best-effort cleanup; always re-raise the original error
      }
      throw err;
    }

    const init: SandboxInit = {
      claimName,
      sandboxName,
      podName,
      namespace: ns,
      customObjectsApi: this.customObjectsApi,
      tracingManager: sandboxTracingManager,
      logger: this.logger,
    };

    const sandbox = new Sandbox(init);

    this.registry.set(`${ns}/${claimName}`, sandbox);
    return sandbox;
  }

  /**
   * Retrieves an existing sandbox handle by claim name.
   * Returns the cached handle if still active, otherwise re-attaches.
   */
  async getSandbox(claimName: string, namespace?: string): Promise<Sandbox> {
    // normalize empty string to defaultNamespace (matches Go behaviour)
    const ns = namespace || this.defaultNamespace;
    const key = `${ns}/${claimName}`;

    // Concurrent callers must share a single attach. Every branch below awaits
    // the API server, so without this two calls for the same claim would both
    // build a handle and the later registry.set() would drop the first one:
    // that handle stays live but untracked, so deleteAll() never closes it and
    // its lifecycle span is never ended. createSandbox() publishes its
    // provision in the same map, so a get for a claim that is still being
    // created joins it instead of attaching a competing handle.
    const inFlight = this.attaching.get(key);
    if (inFlight) {
      return inFlight;
    }
    const attach = this.attachSandbox(claimName, ns, key).finally(() => {
      this.attaching.delete(key);
    });
    this.attaching.set(key, attach);
    return attach;
  }

  private async attachSandbox(
    claimName: string,
    ns: string,
    key: string,
  ): Promise<Sandbox> {
    const existing = this.registry.get(key);
    if (existing?.isActive) {
      // Verify the claim still exists and check if the sandbox name has changed.
      let claimObj: unknown;
      try {
        claimObj = await this.customObjectsApi.getNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace: ns,
          plural: CLAIM_PLURAL_NAME,
          name: claimName,
        });
      } catch (err) {
        // Only a confirmed 404 proves the handle is stale, so only then is it
        // closed and evicted. Any other error (network, auth, 5xx) says nothing
        // about whether the sandbox still exists: closing on those would make a
        // live handle the caller is using permanently inactive because of a
        // transient blip, so leave it registered and let the caller retry.
        if (isK8s404(err)) {
          await existing.closeLocal().catch(() => {});
          this.registry.delete(key);
          throw new SandboxNotFoundError(
            `SandboxClaim '${claimName}' not found in namespace '${ns}'.`,
            { cause: err },
          );
        }
        throw new SandboxError(
          `Failed to verify SandboxClaim '${claimName}' in namespace '${ns}'.`,
          { cause: err },
        );
      }

      // Detect sandboxRef name change since the handle was cached.
      const claimStatus =
        ((claimObj as Record<string, unknown>)?.status as Record<
          string,
          unknown
        >) ?? {};
      const sandboxStatus =
        (claimStatus.sandbox as Record<string, unknown>) ?? {};
      const currentSandboxName = sandboxStatus.name as string | undefined;

      if (!currentSandboxName || currentSandboxName === existing.sandboxName) {
        // Name unchanged (or not yet set) — additionally verify that the
        // underlying Sandbox object still exists. The claim and the sandbox
        // can drift if the Sandbox CR is externally deleted while the claim
        // status has not yet been reconciled.
        try {
          await this.customObjectsApi.getNamespacedCustomObject({
            group: SANDBOX_API_GROUP,
            version: SANDBOX_API_VERSION,
            namespace: ns,
            plural: SANDBOX_PLURAL_NAME,
            name: existing.sandboxName,
          });
        } catch (err) {
          // As above: evict only on a confirmed 404, keep the handle otherwise.
          if (isK8s404(err)) {
            await existing.closeLocal().catch(() => {});
            this.registry.delete(key);
            throw new SandboxNotFoundError(
              `Underlying Sandbox '${existing.sandboxName}' for claim '${claimName}' ` +
                `not found in namespace '${ns}'.`,
              { cause: err },
            );
          }
          throw new SandboxError(
            `Failed to verify Sandbox '${existing.sandboxName}' for claim ` +
              `'${claimName}' in namespace '${ns}'.`,
            { cause: err },
          );
        }
        return existing;
      }

      // The sandbox name has changed; evict and fall through to re-attach below.
      this.logger.info(
        `SandboxClaim '${claimName}' sandboxRef changed ` +
          `from '${existing.sandboxName}' to '${currentSandboxName}'; re-attaching.`,
      );
      await existing.closeLocal().catch(() => {});
      this.registry.delete(key);
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
      // Distinguish 404 (not found) from other K8s errors.
      if (isK8s404(err)) {
        throw new SandboxNotFoundError(
          `SandboxClaim '${claimName}' not found in namespace '${ns}'.`,
          { cause: err },
        );
      }
      throw new SandboxError(
        `Failed to verify SandboxClaim '${claimName}' in namespace '${ns}'.`,
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
    try {
      ({ sandboxName, podName } = await this.waitForSandboxReady(
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
      customObjectsApi: this.customObjectsApi,
      tracingManager: sandboxTracingManager,
      logger: this.logger,
    };

    const sandbox = new Sandbox(init);

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
    const ns = namespace || this.defaultNamespace;
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
   * Returns the WarmPool name referenced by a SandboxClaim, or `undefined` when
   * the claim is not warm-pool-backed (`spec.warmPoolRef` is absent).
   * Throws SandboxNotFoundError if the claim does not exist.
   */
  async getSandboxClaimWarmpoolName(
    claimName: string,
    namespace?: string,
  ): Promise<string | undefined> {
    const ns = namespace || this.defaultNamespace;
    let claimObj: unknown;
    try {
      claimObj = await this.customObjectsApi.getNamespacedCustomObject({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace: ns,
        plural: CLAIM_PLURAL_NAME,
        name: claimName,
      });
    } catch (err) {
      if (isK8s404(err)) {
        throw new SandboxNotFoundError(
          `SandboxClaim '${claimName}' not found in namespace '${ns}'.`,
          { cause: err },
        );
      }
      throw new SandboxError(
        `Failed to get SandboxClaim '${claimName}' in namespace '${ns}'.`,
        { cause: err },
      );
    }
    const spec =
      ((claimObj as Record<string, unknown>)?.spec as Record<
        string,
        unknown
      >) ?? {};
    const warmPoolRef = (spec.warmPoolRef as Record<string, unknown>) ?? {};
    return warmPoolRef.name as string | undefined;
  }

  /**
   * Closes the sandbox handle (if tracked) and deletes the Kubernetes resources.
   */
  async deleteSandbox(claimName: string, namespace?: string): Promise<void> {
    const ns = namespace || this.defaultNamespace;
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
   * Closes and deletes all tracked sandboxes, including the ones still being
   * created. Best-effort.
   */
  async deleteAll(): Promise<void> {
    const snapshot = new Map(this.registry);
    this.registry.clear();

    // Sweep in-flight createSandbox() calls too: their claim is not in the
    // registry yet, so closing handles alone would leave a Sandbox running
    // for a claim nobody holds. Deleting the claim also unblocks the pending
    // create, which fails once its watch observes the deletion.
    // The in-flight promise is captured here, not after the deletions below: a
    // create that finishes mid-sweep drops its own {@link attaching} entry, and
    // looking it up later would miss exactly the handle that needs discarding.
    const pending = [...this.provisioning].map((key) => ({
      key,
      inFlight: this.attaching.get(key),
    }));
    for (const { key } of pending) {
      this.cancelledProvisions.add(key);
    }

    const results = await Promise.allSettled([
      ...[...snapshot.values()].map((sandbox) => sandbox.close()),
      ...pending.map(({ key }) => this.deleteProvisioningClaim(key)),
    ]);

    for (const result of results) {
      if (result.status === "rejected") {
        this.logger.error(`Cleanup failed: ${result.reason}`);
      }
    }

    // Wait for the swept creates to unwind so callers (and the signal handler)
    // do not return while a handle is still being built. A create that resolves
    // anyway raced the sweep: its claim is gone, so drop the handle it
    // registered and end its lifecycle span locally.
    await raceWithTimeout(
      Promise.allSettled(
        pending.map(async ({ key, inFlight }) => {
          const sandbox = await inFlight;
          if (sandbox) {
            this.registry.delete(key);
            await sandbox.closeLocal();
          }
        }),
      ).then(() => undefined),
      CLEANUP_TIMEOUT_MS,
      () => undefined,
    );
  }

  /**
   * Deletes the SandboxClaim of an in-flight createSandbox() directly: no
   * handle exists yet, so there is nothing to close.
   */
  private async deleteProvisioningClaim(key: string): Promise<void> {
    const slashIdx = key.indexOf("/");
    const namespace = key.slice(0, slashIdx);
    const name = key.slice(slashIdx + 1);
    try {
      await raceWithTimeout(
        this.customObjectsApi.deleteNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace,
          plural: CLAIM_PLURAL_NAME,
          name,
        }),
        CLEANUP_TIMEOUT_MS,
        () => {
          throw new Error(
            `SandboxClaim cleanup timed out after ${CLEANUP_TIMEOUT_MS}ms`,
          );
        },
      );
    } catch (err) {
      // The claim may not have reached the apiserver yet; provisionSandbox()
      // deletes it on the cancellation it observes after the create returns.
      if (!isK8s404(err)) {
        throw err;
      }
    }
  }

  /**
   * Registers SIGINT, SIGTERM, and beforeExit handlers to call deleteAll().
   * Returns a function that unregisters the handlers.
   * Idempotent: subsequent calls return a no-op until the returned stop function is called.
   *
   * Signal handling is process-wide, so every client with auto-cleanup enabled
   * shares one set of handlers (see {@link autoCleanupClients}).
   */
  enableAutoCleanup(): () => void {
    if (this.autoCleanupActive) {
      return () => {};
    }
    this.autoCleanupActive = true;
    SandboxClient.autoCleanupClients.add(this);
    SandboxClient.installAutoCleanupHandlers();

    let stopped = false;
    return () => {
      // Guard against a stale stop function from a previous enable/stop cycle
      // unregistering the client all over again.
      if (stopped) {
        return;
      }
      stopped = true;
      this.autoCleanupActive = false;
      SandboxClient.autoCleanupClients.delete(this);
      if (SandboxClient.autoCleanupClients.size === 0) {
        SandboxClient.removeAutoCleanupHandlers();
      }
    };
  }

  /**
   * Clients with auto-cleanup enabled in this process. The handlers below run
   * deleteAll() for all of them and re-raise the signal exactly once, because a
   * per-client handler that re-raised after its own cleanup would terminate the
   * process while another client was still deleting its sandboxes.
   */
  private static readonly autoCleanupClients: Set<SandboxClient> = new Set();

  private static autoCleanupHandlers: {
    beforeExit: () => void;
    signal: (signal: NodeJS.Signals) => void;
  } | null = null;

  private static installAutoCleanupHandlers(): void {
    if (SandboxClient.autoCleanupHandlers) {
      return;
    }

    const beforeExit = () => {
      void SandboxClient.cleanupAllClients();
    };

    const signal = (sig: NodeJS.Signals) => {
      SandboxClient.removeAutoCleanupHandlers();
      const clients = [...SandboxClient.autoCleanupClients];
      SandboxClient.autoCleanupClients.clear();
      for (const client of clients) {
        client.autoCleanupActive = false;
      }
      void Promise.allSettled(
        clients.map((client) => client.deleteAll()),
      ).finally(() => {
        // Re-raise so the default handler terminates the process, now that
        // every client is done deleting.
        process.kill(process.pid, sig);
      });
    };

    SandboxClient.autoCleanupHandlers = { beforeExit, signal };
    process.on("beforeExit", beforeExit);
    process.on("SIGINT", signal);
    process.on("SIGTERM", signal);
  }

  private static removeAutoCleanupHandlers(): void {
    const handlers = SandboxClient.autoCleanupHandlers;
    if (!handlers) {
      return;
    }
    process.off("beforeExit", handlers.beforeExit);
    process.off("SIGINT", handlers.signal);
    process.off("SIGTERM", handlers.signal);
    SandboxClient.autoCleanupHandlers = null;
  }

  private static async cleanupAllClients(): Promise<void> {
    await Promise.allSettled(
      [...SandboxClient.autoCleanupClients].map((client) => client.deleteAll()),
    );
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.deleteAll();
  }

  // -------------------------------------------------------------------------
  // Private: Kubernetes provisioning helpers
  // -------------------------------------------------------------------------

  private async ensureTracer(): Promise<void> {
    if (this.tracerInitialized || !this.enableTracing) return;
    await initializeTracer(this.traceServiceName, this.logger);
    this.tracerInitialized = true;
  }

  private async createClaim(
    claimName: string,
    warmpool: string,
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
          warmPoolRef: { name: warmpool },
        },
      };

      this.logger.info(
        `Creating SandboxClaim '${claimName}' ` +
          `in namespace '${namespace}' ` +
          `using warm pool '${warmpool}'...`,
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

  /**
   * Runs a single watch pass for a SandboxClaim.
   * Returns a WatchPassResult — never rejects (errors are wrapped in the result).
   * A "closed" result means done(null): the caller should re-list and re-watch.
   */
  private watchClaimOnce(
    claimName: string,
    namespace: string,
    remainingMs: number,
    resourceVersion?: string,
  ): Promise<WatchPassResult<string>> {
    return new Promise<WatchPassResult<string>>((resolve) => {
      const watcher = new k8s.Watch(this.kubeConfig);
      let abortController: AbortController | undefined;
      let settled = false;

      // When the remaining budget expires, treat it as a clean close so the
      // outer loop can re-check the deadline and throw SandboxTimeoutError.
      const timer = setTimeout(() => {
        if (!settled) {
          settled = true;
          try {
            abortController?.abort();
          } catch {
            // ignore
          }
          resolve({ type: "closed" });
        }
      }, remainingMs);

      const settle = (result: WatchPassResult<string>) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        try {
          abortController?.abort();
        } catch {
          // ignore
        }
        resolve(result);
      };

      watcher
        .watch(
          `/apis/${CLAIM_API_GROUP}/${CLAIM_API_VERSION}/namespaces/${namespace}/${CLAIM_PLURAL_NAME}`,
          // Start from the resourceVersion the preceding GET observed so no
          // update between that GET and the watch being established is missed.
          { fieldSelector: `metadata.name=${claimName}`, resourceVersion },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ERROR") {
              // The apiserver reports a stale/compacted resourceVersion as an
              // ERROR event (typically 410 Gone). Treat it as a clean close so
              // the caller re-GETs and restarts from a fresh version.
              settle({ type: "closed" });
              return;
            }
            if (type === "ADDED" || type === "MODIFIED") {
              const status = (obj.status as Record<string, unknown>) ?? {};
              const conditions =
                (status.conditions as Array<Record<string, string>>) ?? [];
              try {
                inspectClaimConditions(conditions);
              } catch (err) {
                settle({
                  type: "error",
                  error: err instanceof Error ? err : new Error(String(err)),
                });
                return;
              }
              const sandboxStatus =
                (status.sandbox as Record<string, unknown>) ?? {};
              const name = sandboxStatus.name as string | undefined;
              if (name) {
                this.logger.info(
                  `Resolved sandbox name '${name}' from claim status.`,
                );
                settle({ type: "resolved", value: name });
              }
            } else if (type === "DELETED") {
              settle({
                type: "error",
                error: new SandboxMetadataError(
                  `SandboxClaim '${claimName}' was deleted while waiting for it to be resolved.`,
                ),
              });
            }
          },
          (err) => {
            if (isWatchCleanClose(err)) {
              // Clean close (done(null) / AbortError / 30s TimeoutError / 410
              // Gone): the outer loop re-GETs and re-watches until the deadline.
              settle({ type: "closed" });
            } else {
              settle({
                type: "error",
                error: err instanceof Error ? err : new Error(String(err)),
              });
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
          settle({
            type: "error",
            error: err instanceof Error ? err : new Error(String(err)),
          });
        });
    });
  }

  /**
   * Resolves the actual Sandbox name from a SandboxClaim's status.
   * Uses an initial GET followed by repeated watch passes with re-list on clean close,
   * mirroring the Go client's loop pattern.
   */
  private async resolveSandboxName(
    claimName: string,
    namespace: string,
    timeoutMs: number,
  ): Promise<string> {
    this.logger.info(`Resolving sandbox name from claim '${claimName}'...`);

    const deadline = Date.now() + timeoutMs;
    let backoffMs = 100;
    const MAX_BACKOFF_MS = 5_000;

    while (true) {
      // Re-list: check if claim is already resolved (initial GET or re-list after clean close).
      let fetchedClaim: unknown;
      try {
        fetchedClaim = await this.customObjectsApi.getNamespacedCustomObject({
          group: CLAIM_API_GROUP,
          version: CLAIM_API_VERSION,
          namespace,
          plural: CLAIM_PLURAL_NAME,
          name: claimName,
        });
      } catch (err) {
        // 404 means the claim is gone — fail immediately, do not fall through to watch.
        if (isK8s404(err)) {
          throw new SandboxNotFoundError(
            `SandboxClaim '${claimName}' not found in namespace '${namespace}'.`,
            { cause: err },
          );
        }
        // Non-404 (transient network error, etc.) — fall through to watch.
      }

      // resourceVersion from the GET (if any) so the watch resumes exactly
      // where the GET left off and no interim update is dropped.
      let resourceVersion: string | undefined;
      if (fetchedClaim !== undefined) {
        const claimObj = fetchedClaim as Record<string, unknown>;
        const metadata = (claimObj?.metadata as Record<string, unknown>) ?? {};
        resourceVersion = metadata.resourceVersion as string | undefined;
        const status = (claimObj?.status as Record<string, unknown>) ?? {};
        const conditions =
          (status.conditions as Array<Record<string, string>>) ?? [];
        inspectClaimConditions(conditions); // throws SandboxTemplateNotFoundError / SandboxWarmPoolNotFoundError / SandboxClaimFailedError
        const sandboxStatus = (status.sandbox as Record<string, unknown>) ?? {};
        const name = sandboxStatus.name as string | undefined;
        if (name) {
          this.logger.info(
            `Resolved sandbox name '${name}' from claim status (GET).`,
          );
          return name;
        }
      }

      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        throw new SandboxTimeoutError(
          `Sandbox claim '${claimName}' did not resolve within ${Math.floor(timeoutMs / 1000)} seconds.`,
        );
      }

      // Single watch pass.
      const result = await this.watchClaimOnce(
        claimName,
        namespace,
        remaining,
        resourceVersion,
      );
      if (result.type === "resolved") {
        return result.value;
      }
      if (result.type === "error") {
        throw result.error;
      }

      // result.type === "closed": done(null) — re-list.
      const remainingAfterWatch = deadline - Date.now();
      if (remainingAfterWatch <= 0) {
        throw new SandboxTimeoutError(
          `Sandbox claim '${claimName}' did not resolve within ${Math.floor(timeoutMs / 1000)} seconds.`,
        );
      }
      this.logger.info(
        `Claim watch closed cleanly; re-listing after backoff (${backoffMs}ms)...`,
      );
      await new Promise<void>((r) =>
        setTimeout(r, Math.min(backoffMs, remainingAfterWatch)),
      );
      backoffMs = Math.min(backoffMs * 2, MAX_BACKOFF_MS);
    }
  }

  /**
   * Runs a single watch pass for a Sandbox resource.
   * Returns a WatchPassResult — never rejects (errors are wrapped in the result).
   * A "closed" result means done(null): the caller should re-list and re-watch.
   */
  private watchSandboxOnce(
    sandboxName: string,
    namespace: string,
    remainingMs: number,
    resourceVersion?: string,
  ): Promise<
    WatchPassResult<{ podName: string; annotations: Record<string, string> }>
  > {
    return new Promise((resolve) => {
      const watcher = new k8s.Watch(this.kubeConfig);
      let abortController: AbortController | undefined;
      let settled = false;

      const timer = setTimeout(() => {
        if (!settled) {
          settled = true;
          try {
            abortController?.abort();
          } catch {
            // ignore
          }
          resolve({ type: "closed" });
        }
      }, remainingMs);

      const settle = (
        result: WatchPassResult<{
          podName: string;
          annotations: Record<string, string>;
        }>,
      ) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        try {
          abortController?.abort();
        } catch {
          // ignore
        }
        resolve(result);
      };

      watcher
        .watch(
          `/apis/${SANDBOX_API_GROUP}/${SANDBOX_API_VERSION}/namespaces/${namespace}/${SANDBOX_PLURAL_NAME}`,
          // Start from the resourceVersion the preceding GET observed so a
          // readiness transition between that GET and the watch is not missed.
          { fieldSelector: `metadata.name=${sandboxName}`, resourceVersion },
          (type: string, obj: Record<string, unknown>) => {
            if (type === "ERROR") {
              // Stale/compacted resourceVersion (typically 410 Gone) → clean
              // close so the caller re-GETs and restarts from a fresh version.
              settle({ type: "closed" });
              return;
            }
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
                  settle({
                    type: "error",
                    error: new SandboxMetadataError(
                      "Could not determine sandbox name from sandbox object.",
                    ),
                  });
                  return;
                }
                this.logger.info(`Sandbox ${resolvedName} is ready.`);

                const annotations =
                  (metadata.annotations as Record<string, string>) ?? {};
                const podNameAnnotation = annotations[POD_NAME_ANNOTATION];
                const podName = podNameAnnotation ?? resolvedName;
                if (podNameAnnotation) {
                  this.logger.info(
                    `Found pod name from annotation: ${podName}`,
                  );
                }

                settle({ type: "resolved", value: { podName, annotations } });
              }
            } else if (type === "DELETED") {
              settle({
                type: "error",
                error: new SandboxNotFoundError(
                  `Sandbox '${sandboxName}' was deleted while waiting for it to become ready.`,
                ),
              });
            }
          },
          (err) => {
            if (isWatchCleanClose(err)) {
              // Clean close (done(null) / AbortError / 30s TimeoutError / 410
              // Gone): the outer loop re-GETs and re-watches until the deadline.
              settle({ type: "closed" });
            } else {
              settle({
                type: "error",
                error: err instanceof Error ? err : new Error(String(err)),
              });
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
          settle({
            type: "error",
            error: err instanceof Error ? err : new Error(String(err)),
          });
        });
    });
  }

  /**
   * Watches a Sandbox resource until it becomes Ready.
   * Uses an initial GET followed by repeated watch passes with re-list on clean close,
   * mirroring the Go client's loop pattern.
   */
  private async watchForSandboxReady(
    sandboxName: string,
    namespace: string,
    timeoutMs: number,
  ): Promise<{ podName: string; annotations: Record<string, string> }> {
    this.logger.info("Watching for Sandbox to become ready...");

    const deadline = Date.now() + timeoutMs;
    let backoffMs = 100;
    const MAX_BACKOFF_MS = 5_000;

    let resourceVersion: string | undefined;
    while (true) {
      // Re-list: check if sandbox is already Ready (initial GET or re-list after clean close).
      try {
        const existing = await this.customObjectsApi.getNamespacedCustomObject({
          group: SANDBOX_API_GROUP,
          version: SANDBOX_API_VERSION,
          namespace,
          plural: SANDBOX_PLURAL_NAME,
          name: sandboxName,
        });
        const obj = existing as Record<string, unknown>;
        const objMetadata = (obj?.metadata as Record<string, unknown>) ?? {};
        resourceVersion = objMetadata.resourceVersion as string | undefined;
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
            this.logger.info(`Sandbox ${resolvedName} is already ready (GET).`);
            const annotations =
              (metadata.annotations as Record<string, string>) ?? {};
            const podNameAnnotation = annotations[POD_NAME_ANNOTATION];
            const podName = podNameAnnotation ?? resolvedName;
            if (podNameAnnotation) {
              this.logger.info(`Found pod name from annotation: ${podName}`);
            }
            return { podName, annotations };
          }
        }
      } catch {
        // Sandbox may not exist yet or transient error — fall through to watch.
      }

      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        throw new SandboxTimeoutError(
          `Sandbox '${sandboxName}' did not become ready within ${Math.floor(timeoutMs / 1000)} seconds.`,
        );
      }

      // Single watch pass.
      const result = await this.watchSandboxOnce(
        sandboxName,
        namespace,
        remaining,
        resourceVersion,
      );
      if (result.type === "resolved") {
        return result.value;
      }
      if (result.type === "error") {
        throw result.error;
      }

      // result.type === "closed": done(null) — re-list.
      const remainingAfterWatch = deadline - Date.now();
      if (remainingAfterWatch <= 0) {
        throw new SandboxTimeoutError(
          `Sandbox '${sandboxName}' did not become ready within ${Math.floor(timeoutMs / 1000)} seconds.`,
        );
      }
      this.logger.info(
        `Sandbox watch closed cleanly; re-listing after backoff (${backoffMs}ms)...`,
      );
      await new Promise<void>((r) =>
        setTimeout(r, Math.min(backoffMs, remainingAfterWatch)),
      );
      backoffMs = Math.min(backoffMs * 2, MAX_BACKOFF_MS);
    }
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
      const remainingMs = totalTimeoutMs - elapsed;
      // If claim resolution consumed the entire budget, fail fast with a
      // clear timeout error rather than passing 0 ms to watchForSandboxReady.
      if (remainingMs <= 0) {
        throw new SandboxTimeoutError(
          `Sandbox name resolution for claim '${claimName}' consumed the entire timeout budget.`,
        );
      }
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
