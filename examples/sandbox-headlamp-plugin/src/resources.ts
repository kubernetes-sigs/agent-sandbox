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

interface SandboxSpec {
  operatingMode?: string;
  shutdownTime?: string;
  shutdownPolicy?: string;
  podTemplate?: {
    spec?: {
      containers?: Array<{ name?: string; image?: string }>;
    };
  };
  service?: boolean;
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
    return this.status.conditions?.find(condition => condition.type === 'Ready');
  }

  get readyStatus() {
    return this.readyCondition?.status || '-';
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
}

export interface SandboxWarmPoolData extends KubeObjectInterface {
  spec: {
    replicas?: number;
    sandboxTemplateRef: { name: string };
    updateStrategy?: { type?: string };
  };
  status?: {
    replicas?: number;
    readyReplicas?: number;
    selector?: string;
    observedGeneration?: number;
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
}
