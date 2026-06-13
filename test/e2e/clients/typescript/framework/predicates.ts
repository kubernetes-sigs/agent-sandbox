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

import * as k8s from "@kubernetes/client-node";

/**
 * Predicate to check if a Deployment has at least minReady available replicas.
 */
export function deploymentReady(
  minReady: number = 1,
): (obj: k8s.V1Deployment) => boolean {
  return (obj: k8s.V1Deployment): boolean => {
    if (obj.status) {
      const availableReplicas = obj.status.availableReplicas ?? 0;
      return availableReplicas >= minReady;
    }
    return false;
  };
}

/**
 * Predicate to check if a SandboxWarmPool (CR) has all the required number of ready sandboxes.
 */
export function warmPoolReady(): (
  obj: Record<string, any>,
) => boolean {
  return (obj: Record<string, any>): boolean => {
    const status = obj?.status ?? {};
    const readyReplicas = status.readyReplicas ?? 0;
    const replicas = obj?.spec?.replicas ?? 0;
    return readyReplicas === replicas;
  };
}

/**
 * Predicate to check if a Gateway has an address.
 */
export function gatewayAddressReady(): (
  obj: Record<string, any>,
) => boolean {
  return (obj: Record<string, any>): boolean => {
    const status = obj?.status ?? {};
    const addresses = status.addresses ?? [];
    return addresses.length > 0;
  };
}
