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

// Total attempt count for idempotent operations (matches Go client's maxAttempts=6).
// With the loop `for (let attempt = 0; attempt < MAX_RETRIES; attempt++)` this gives
// attempts 0–5, i.e. up to 5 retries after the first attempt.
// Non-idempotent callers (POST /execute, /upload, /agent) pass maxRetries: 1 explicitly.
export const MAX_RETRIES = 6;
export const BACKOFF_FACTOR = 0.5;
export const RETRY_STATUS_CODES = [500, 502, 503, 504];

// Maximum bytes to drain from a response body before retrying (allows TCP connection reuse)
export const MAX_DRAIN_BYTES = 4096;

// Maximum bytes of response body to include in SandboxRequestError.body
export const MAX_ERROR_BODY_BYTES = 512;

// Number of port-forward reconnect attempts before giving up
export const MAX_RECONNECT_ATTEMPTS = 3;

// Per-attempt timeout in milliseconds (independent of the overall request timeout)
export const PER_ATTEMPT_TIMEOUT_MS = 30_000;

// Maximum number of gateway watch reconnects within a single waitForGatewayIp call
export const MAX_GATEWAY_REWATCH = 10;

// Response / request size limits (matches Go client constants)
export const MAX_EXECUTION_RESPONSE_SIZE = 16 * 1024 * 1024; // 16 MB: run/agent stdout+stderr
export const MAX_METADATA_RESPONSE_SIZE = 8 * 1024 * 1024; //  8 MB: list/exists JSON
export const MAX_DOWNLOAD_SIZE = 256 * 1024 * 1024; // 256 MB: file download
export const MAX_UPLOAD_SIZE = 256 * 1024 * 1024; // 256 MB: file upload (pre-check)
