/*
 * Copyright 2026 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { KubeObject, KubeObjectInterface } from '@kinvolk/headlamp-plugin/lib/k8s/cluster';

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export function readyCondition(conditions?: Condition[]) {
  return conditions?.find(condition => condition.type === 'Ready');
}

export function readyStatus(conditions?: Condition[]) {
  return readyCondition(conditions)?.status || '-';
}

type SandboxOperatingMode = 'Running' | 'Suspended';
type SandboxWarmPoolUpdateStrategy = 'Recreate' | 'OnReplenish';
type NetworkPolicyManagement = 'Managed' | 'Unmanaged';
type EnvVarsInjectionPolicy = 'Allowed' | 'Overrides' | 'Disallowed';
type VolumeClaimTemplatesPolicy = 'Allowed' | 'Overrides' | 'Disallowed';

// These interfaces intentionally model only fields consumed by the read-only views.
// They are projections of the CRDs, not replacements for the generated API types.
interface SandboxSpec {
  operatingMode?: SandboxOperatingMode;
}

export interface SandboxData extends KubeObjectInterface {
  spec: SandboxSpec;
  status?: {
    serviceFQDN?: string;
    service?: string;
    conditions?: Condition[];
    podIPs?: string[];
    nodeName?: string;
  };
}

export class Sandbox extends KubeObject<SandboxData> {
  static apiVersion = 'agents.x-k8s.io/v1beta1';
  static kind = 'Sandbox';
  static apiName = 'sandboxes';
  static isNamespaced = true;

  static get detailsRoute() {
    return '/agent-sandbox/sandboxes/:namespace/:name';
  }

  get spec() {
    return this.jsonData.spec;
  }

  get status() {
    return this.jsonData.status || {};
  }

  get readyCondition() {
    return readyCondition(this.status.conditions);
  }

  get readyStatus() {
    return readyStatus(this.status.conditions);
  }

  get operatingMode() {
    return this.spec.operatingMode ?? 'Running';
  }

  get podIP() {
    return this.status.podIPs?.join(', ') || '-';
  }

  get launchType() {
    return this.metadata.labels?.['agents.x-k8s.io/launch-type'] || '-';
  }

  get backingPodName() {
    return this.metadata.annotations?.['agents.x-k8s.io/pod-name'] || this.metadata.name;
  }
}

export interface SandboxClaimData extends KubeObjectInterface {
  spec: {
    warmPoolRef: { name: string };
    lifecycle?: {
      shutdownTime?: string;
      shutdownPolicy?: string;
      ttlSecondsAfterFinished?: number;
    };
  };
  status?: {
    conditions?: Condition[];
    sandbox?: {
      name?: string;
      podIPs?: string[];
      serviceFQDN?: string;
    };
  };
}

export class SandboxClaim extends KubeObject<SandboxClaimData> {
  static apiVersion = 'extensions.agents.x-k8s.io/v1beta1';
  static kind = 'SandboxClaim';
  static apiName = 'sandboxclaims';
  static isNamespaced = true;

  static get detailsRoute() {
    return '/agent-sandbox/claims/:namespace/:name';
  }

  get spec() {
    return this.jsonData.spec;
  }

  get status() {
    return this.jsonData.status || {};
  }

  get assignedSandboxName() {
    return this.status.sandbox?.name || '-';
  }

  get readyStatus() {
    return readyStatus(this.status.conditions);
  }
}

export interface SandboxWarmPoolData extends KubeObjectInterface {
  spec: {
    replicas?: number;
    sandboxTemplateRef: { name: string };
    updateStrategy?: { type?: SandboxWarmPoolUpdateStrategy };
  };
  status?: {
    replicas?: number;
    readyReplicas?: number;
    selector?: string;
    observedGeneration?: number;
  };
}

export interface SandboxTemplateData extends KubeObjectInterface {
  spec: {
    service?: boolean;
    networkPolicyManagement?: NetworkPolicyManagement;
    envVarsInjectionPolicy?: EnvVarsInjectionPolicy;
    volumeClaimTemplatesPolicy?: VolumeClaimTemplatesPolicy;
  };
}

export class SandboxWarmPool extends KubeObject<SandboxWarmPoolData> {
  static apiVersion = 'extensions.agents.x-k8s.io/v1beta1';
  static kind = 'SandboxWarmPool';
  static apiName = 'sandboxwarmpools';
  static isNamespaced = true;

  static get detailsRoute() {
    return '/agent-sandbox/warmpools/:namespace/:name';
  }

  get spec() {
    return this.jsonData.spec;
  }

  get status() {
    return this.jsonData.status || {};
  }

  get desiredReplicas() {
    return this.jsonData.spec.replicas ?? 1;
  }

  get readyReplicas() {
    return this.status.readyReplicas ?? 0;
  }

  get updateStrategy() {
    return this.spec.updateStrategy?.type ?? 'OnReplenish';
  }

  get templateName() {
    return this.spec.sandboxTemplateRef?.name || '-';
  }
}

export class SandboxTemplate extends KubeObject<SandboxTemplateData> {
  static apiVersion = 'extensions.agents.x-k8s.io/v1beta1';
  static kind = 'SandboxTemplate';
  static apiName = 'sandboxtemplates';
  static isNamespaced = true;

  static get detailsRoute() {
    return '/agent-sandbox/templates/:namespace/:name';
  }

  get spec() {
    return this.jsonData.spec;
  }

  get networkPolicyManagement() {
    return this.spec.networkPolicyManagement ?? 'Managed';
  }

  get envVarsInjectionPolicy() {
    return this.spec.envVarsInjectionPolicy ?? 'Disallowed';
  }

  get volumeClaimTemplatesPolicy() {
    return this.spec.volumeClaimTemplatesPolicy ?? 'Disallowed';
  }

  get serviceState() {
    if (this.spec.service === undefined) {
      return 'Unchanged';
    }
    return this.spec.service ? 'Enabled' : 'Disabled';
  }
}
