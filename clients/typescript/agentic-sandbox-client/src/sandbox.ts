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
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  CLEANUP_TIMEOUT_MS,
} from "./constants.js";
import { isK8s404, SandboxError } from "./exceptions.js";
import { noopLogger } from "./logger.js";
import type { TracerManager } from "./trace-manager.js";
import type { Logger } from "./types.js";

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
  tracingManager: TracerManager | null;
  logger?: Logger;
}

/**
 * A claimed Sandbox resource handle: stable identity (claim / sandbox / pod
 * names + namespace) plus lifecycle (`close()` / `closeLocal()`), with no
 * transport of its own. Obtain instances via SandboxClient.createSandbox() or
 * getSandbox(). Connectivity to the sandbox runtime is provided separately
 * (see issue #977).
 */
export class Sandbox {
  readonly claimName: string;
  readonly sandboxName: string;
  readonly podName: string;
  readonly namespace: string;

  protected readonly tracingManager: TracerManager | null;
  protected readonly customObjectsApi: k8s.CustomObjectsApi;
  protected readonly logger: Logger;

  private _isClosed = false;
  private _inflightCount = 0;
  private _drainResolvers: Array<() => void> = [];

  constructor(init: SandboxInit) {
    this.claimName = init.claimName;
    this.sandboxName = init.sandboxName;
    this.podName = init.podName;
    this.namespace = init.namespace;
    this.customObjectsApi = init.customObjectsApi;
    this.tracingManager = init.tracingManager;
    this.logger = init.logger ?? noopLogger;
  }

  /**
   * Returns true if the handle has not been closed.
   */
  get isActive(): boolean {
    return !this._isClosed;
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

    if (this.tracingManager) {
      try {
        this.tracingManager.endLifecycleSpan();
      } catch (err) {
        this.logger.error(`Failed to end tracing span: ${err}`);
      }
    }
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

    // Drain in-flight work; give up after CLEANUP_TIMEOUT_MS so close() is bounded.
    await raceWithTimeout(
      this.drainInflight(),
      CLEANUP_TIMEOUT_MS,
      () => undefined,
    );

    await this.closeLocal();

    if (this.claimName) {
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
        // A claim that is already gone is the desired end state.
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
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  /**
   * Resolves once no work is in flight. The resource-layer handle never starts
   * in-flight work of its own; this is the seam a connectivity layer (issue
   * #977) hooks into so close() can wait for outstanding requests to settle.
   */
  private drainInflight(): Promise<void> {
    if (this._inflightCount === 0) return Promise.resolve();
    return new Promise<void>((resolve) => {
      this._drainResolvers.push(resolve);
    });
  }
}
