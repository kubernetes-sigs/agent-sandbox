/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import * as crypto from "node:crypto";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";
import * as k8s from "@kubernetes/client-node";
import fetch, { type RequestInit } from "node-fetch";
import {
  deploymentReady,
  gatewayAddressReady,
  warmPoolReady,
} from "./predicates.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
// Project root is 5 levels up from test/e2e/clients/typescript/framework/
const PROJECT_ROOT = path.resolve(__dirname, "../../../../..");
const DEFAULT_KUBECONFIG_PATH = path.join(PROJECT_ROOT, "bin/KUBECONFIG");
const DEFAULT_TIMEOUT_SECONDS = 120;

/**
 * Context for E2E tests, managing Kubernetes interactions.
 */
export class TestContext {
  private kubeconfigPath: string;
  private _kubeConfig: k8s.KubeConfig | null = null;
  private _coreV1Api: k8s.CoreV1Api | null = null;
  private _appsV1Api: k8s.AppsV1Api | null = null;
  namespace: string | null = null;

  constructor(kubeconfigPath?: string) {
    this.kubeconfigPath = kubeconfigPath ?? process.env["KUBECONFIG"] ??
      DEFAULT_KUBECONFIG_PATH;
  }

  get kubeConfig(): k8s.KubeConfig {
    if (!this._kubeConfig) {
      this._kubeConfig = new k8s.KubeConfig();
      this._kubeConfig.loadFromFile(this.kubeconfigPath);
    }
    return this._kubeConfig;
  }

  get coreV1Api(): k8s.CoreV1Api {
    if (!this._coreV1Api) {
      this._coreV1Api = this.kubeConfig.makeApiClient(k8s.CoreV1Api);
    }
    return this._coreV1Api;
  }

  get appsV1Api(): k8s.AppsV1Api {
    if (!this._appsV1Api) {
      this._appsV1Api = this.kubeConfig.makeApiClient(k8s.AppsV1Api);
    }
    return this._appsV1Api;
  }

  /**
   * Creates a temporary namespace for testing.
   */
  async createTempNamespace(prefix: string = "test-"): Promise<string> {
    const namespaceName = `${prefix}${crypto.randomBytes(4).toString("hex")}`;
    await this.coreV1Api.createNamespace({
      body: {
        apiVersion: "v1",
        kind: "Namespace",
        metadata: { name: namespaceName },
      },
    });
    this.namespace = namespaceName;
    console.log(`Created namespace: ${this.namespace}`);
    return this.namespace;
  }

  /**
   * Deletes the specified namespace.
   */
  async deleteNamespace(namespace?: string): Promise<void> {
    const ns = namespace ?? this.namespace;
    if (!ns) return;

    try {
      await this.coreV1Api.deleteNamespace({ name: ns });
      console.log(`Deleted namespace: ${ns}`);
      if (this.namespace === ns) {
        this.namespace = null;
      }
    } catch (e: unknown) {
      const status = (e as { statusCode?: number }).statusCode;
      if (status === 404) {
        console.log(`Namespace ${ns} not found, skipping deletion.`);
      } else {
        throw e;
      }
    }
  }

  /**
   * Applies the given manifest text to the cluster using kubectl.
   */
  applyManifestText(manifestText: string, namespace?: string): void {
    const ns = namespace ?? this.namespace;
    if (!ns) {
      throw new Error(
        "Namespace must be provided or created before applying manifests.",
      );
    }

    try {
      const result = execFileSync(
        "kubectl",
        ["apply", "-f", "-", "-n", ns],
        {
          input: manifestText,
          encoding: "utf-8",
          stdio: ["pipe", "pipe", "pipe"],
          timeout: 30_000,
        },
      );
      if (result) {
        console.log(result);
      }
    } catch (e: unknown) {
      const err = e as { stderr?: string; status?: number };
      const errorMsg = err.stderr?.trim() ?? "unknown error";
      console.error(`kubectl apply failed: ${errorMsg}`);
      throw new Error(`Failed to apply manifest: ${errorMsg}`);
    }
  }

  /**
   * Waits for a Kubernetes object to satisfy a given predicate function using the Watch API.
   */
  async waitForObject<T>(
    watchPath: string,
    name: string,
    namespace: string,
    predicateFn: (obj: T) => boolean,
    timeout: number = DEFAULT_TIMEOUT_SECONDS,
  ): Promise<boolean> {
    const watcher = new k8s.Watch(this.kubeConfig);
    const startMs = Date.now();
    const timeoutMs = timeout * 1000;

    // Initial GET: check if already satisfied and capture resourceVersion to
    // avoid missing events that occur between the GET and watch establishment.
    let watchResourceVersion: string | undefined;
    let initialObj: (T & { metadata?: { resourceVersion?: string } }) | undefined;
    try {
      const cluster = this.kubeConfig.getCurrentCluster();
      if (cluster?.server) {
        const serverBase = cluster.server.replace(/\/$/, "");
        const fetchOpts = (await this.kubeConfig.applyToFetchOptions(
          {},
        )) as RequestInit;
        const timeoutSignal = AbortSignal.timeout(10_000);
        fetchOpts.signal = fetchOpts.signal
          ? AbortSignal.any([fetchOpts.signal as AbortSignal, timeoutSignal])
          : timeoutSignal;
        const resp = await fetch(
          `${serverBase}${watchPath}/${name}`,
          fetchOpts,
        );
        if (resp.ok) {
          initialObj = (await resp.json()) as T & {
            metadata?: { resourceVersion?: string };
          };
          watchResourceVersion = initialObj.metadata?.resourceVersion;
        } else {
          // Drain the body to release the TCP socket back to the keep-alive pool.
          resp.body?.resume();
        }
      }
    } catch {
      // Transient network/HTTP error — fall through to watch.
    }

    // Evaluate the predicate outside the network catch block so that
    // errors thrown by predicateFn propagate to the caller rather than
    // being silently swallowed as if they were transient GET failures.
    if (initialObj != null && predicateFn(initialObj)) {
      console.log(
        `Object ${name} already satisfied predicate (initial GET).`,
      );
      return true;
    }

    return new Promise<boolean>((resolve, reject) => {
      // Track whether the promise has been settled (resolved or rejected)
      // to prevent duplicate resolve/reject calls.
      let settled = false;
      let timedOut = false;

      let abortController: AbortController | undefined;
      let timer: ReturnType<typeof setTimeout>;

      const abortWatch = () => {
        if (abortController) {
          try {
            abortController.abort();
          } catch {
            // ignore
          }
        }
      };

      const cleanup = () => {
        clearTimeout(timer);
        abortWatch();
      };

      const elapsed = Date.now() - startMs;
      const remainingMs = Math.max(0, timeoutMs - elapsed);
      timer = setTimeout(() => {
        if (settled) return;
        settled = true;
        timedOut = true;
        cleanup();
        try {
          const resourceType = watchPath.split("/").pop() ?? "object";
          const desc = execFileSync(
            "kubectl",
            ["describe", resourceType, name, "-n", namespace],
            { encoding: "utf-8", stdio: ["ignore", "pipe", "pipe"], timeout: 5_000 },
          );
          const pods = execFileSync(
            "kubectl",
            ["get", "pods", "-n", namespace, "-o", "wide"],
            { encoding: "utf-8", stdio: ["ignore", "pipe", "pipe"], timeout: 5_000 },
          );
          console.error(
            `[waitForObject timeout ${name}] describe:\n${desc}`,
          );
          console.error(
            `[waitForObject timeout ${name}] pods:\n${pods}`,
          );
        } catch (dumpErr) {
          console.error(
            `[waitForObject timeout ${name}] dump failed:`,
            dumpErr,
          );
        }
        reject(
          new Error(
            `Object ${name} did not satisfy predicate within ${timeout} seconds.`,
          ),
        );
      }, remainingMs);

      watcher
        .watch(
          watchPath,
          {
            fieldSelector: `metadata.name=${name}`,
            // Pass the captured resourceVersion to ensure no events are missed
            // between the initial GET and watch establishment. Fall back to "0"
            // (replay from the API server's watch cache) when the GET failed or
            // returned a non-ok status — "0" ensures an ADDED event is delivered
            // for objects that already exist.
            resourceVersion: watchResourceVersion ?? "0",
          },
          (type: string, obj: T) => {
            console.log(
              `[watch ${name}] event=${type} status=${JSON.stringify((obj as { status?: unknown })?.status)}`,
            );
            if (settled) return;
            let matched: boolean;
            try {
              matched = predicateFn(obj);
            } catch (predicateErr) {
              // predicateFn threw — propagate to the caller. Without this guard
              // the k8s Watch library's own try/catch would silently discard the
              // exception, leaving the Promise to hang until the timeout fires.
              settled = true;
              cleanup();
              reject(predicateErr);
              return;
            }
            if (matched) {
              console.log(
                `Object ${name} satisfied predicate on event type ${type}.`,
              );
              settled = true;
              cleanup();
              resolve(true);
            }
          },
          (err?: unknown) => {
            cleanup();
            // Only reject if we haven't already settled.
            // When we abort the watch after success or timeout, the
            // doneCallback receives an abort error which we should ignore.
            if (err && !settled) {
              settled = true;
              reject(
                new Error(
                  `Watch error for ${name}: ${err instanceof Error ? err.message : String(err)
                  }`,
                ),
              );
            }
            if (!err && !settled) {
              settled = true;
              reject(
                new Error(
                  `Watch for ${name} ended before predicate was satisfied.`,
                ),
              );
            }
          },
        )
        .then((ac) => {
          abortController = ac;
          // Abort if already settled (timeout or predicate satisfied) before
          // the watch's AbortController was returned to us.
          if (timedOut || settled) {
            abortWatch();
          }
        })
        .catch((err) => {
          cleanup();
          if (!settled) {
            settled = true;
            reject(err);
          }
        });
    });
  }

  /**
   * Waits for a Deployment to have at least minReady available replicas.
   */
  async waitForDeploymentReady(
    name: string,
    namespace?: string,
    minReady: number = 1,
    timeout: number = DEFAULT_TIMEOUT_SECONDS,
  ): Promise<boolean> {
    const ns = namespace ?? this.namespace;
    if (!ns) throw new Error("Namespace must be provided.");

    return this.waitForObject<k8s.V1Deployment>(
      `/apis/apps/v1/namespaces/${ns}/deployments`,
      name,
      ns,
      deploymentReady(minReady),
      timeout,
    );
  }

  /**
   * Waits for a SandboxWarmPool to have all the required number of ready sandboxes.
   */
  async waitForWarmPoolReady(
    name: string,
    namespace?: string,
    timeout: number = DEFAULT_TIMEOUT_SECONDS,
  ): Promise<boolean> {
    const ns = namespace ?? this.namespace;
    if (!ns) throw new Error("Namespace must be provided.");

    return this.waitForObject<Record<string, any>>(
      `/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/${ns}/sandboxwarmpools`,
      name,
      ns,
      warmPoolReady(),
      timeout,
    );
  }

  /**
   * Waits for a Gateway to have an address in its status.
   */
  async waitForGatewayAddress(
    name: string,
    namespace?: string,
    timeout: number = DEFAULT_TIMEOUT_SECONDS,
  ): Promise<boolean> {
    const ns = namespace ?? this.namespace;
    if (!ns) throw new Error("Namespace must be provided.");

    return this.waitForObject<Record<string, any>>(
      `/apis/gateway.networking.k8s.io/v1/namespaces/${ns}/gateways`,
      name,
      ns,
      gatewayAddressReady(),
      timeout,
    );
  }
}
