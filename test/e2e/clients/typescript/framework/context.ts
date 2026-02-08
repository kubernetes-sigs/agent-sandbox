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
import { execFileSync } from "node:child_process";
import * as k8s from "@kubernetes/client-node";
import {
  deploymentReady,
  warmPoolReady,
  gatewayAddressReady,
} from "./predicates.js";

const DEFAULT_KUBECONFIG_PATH = "bin/KUBECONFIG";
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
    this.kubeconfigPath =
      kubeconfigPath ?? process.env["KUBECONFIG"] ?? DEFAULT_KUBECONFIG_PATH;
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
    const timeoutMs = timeout * 1000;

    return new Promise<boolean>((resolve, reject) => {
      const timer = setTimeout(() => {
        reject(
          new Error(
            `Object ${name} did not satisfy predicate within ${timeout} seconds.`,
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

      watcher
        .watch(
          watchPath,
          { fieldSelector: `metadata.name=${name}` },
          (type: string, obj: T) => {
            if (predicateFn(obj)) {
              console.log(
                `Object ${name} satisfied predicate on event type ${type}.`,
              );
              cleanup();
              resolve(true);
            }
          },
          (err?: unknown) => {
            cleanup();
            if (err) {
              reject(
                new Error(
                  `Watch error for ${name}: ${err instanceof Error ? err.message : String(err)}`,
                ),
              );
            }
          },
        )
        .then((ac) => {
          abortController = ac;
        })
        .catch((err) => {
          cleanup();
          reject(err);
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
      `/apis/extensions.agents.x-k8s.io/v1alpha1/namespaces/${ns}/sandboxwarmpools`,
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
