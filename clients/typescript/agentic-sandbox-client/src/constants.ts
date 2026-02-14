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

export const GATEWAY_API_GROUP = "gateway.networking.k8s.io";
export const GATEWAY_API_VERSION = "v1";
export const GATEWAY_PLURAL = "gateways";

export const CLAIM_API_GROUP = "extensions.agents.x-k8s.io";
export const CLAIM_API_VERSION = "v1alpha1";
export const CLAIM_PLURAL_NAME = "sandboxclaims";

export const SANDBOX_API_GROUP = "agents.x-k8s.io";
export const SANDBOX_API_VERSION = "v1alpha1";
export const SANDBOX_PLURAL_NAME = "sandboxes";

export const POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name";

export const MAX_RETRIES = 5;
export const BACKOFF_FACTOR = 0.5;
export const RETRY_STATUS_CODES = [500, 502, 503, 504];
