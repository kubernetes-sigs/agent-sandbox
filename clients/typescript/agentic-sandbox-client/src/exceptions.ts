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

/**
 * Base class for all sandbox-related errors.
 */
export class SandboxError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = this.constructor.name;
  }
}

/**
 * Raised when the sandbox or sandbox claim cannot be found or was deleted.
 */
export class SandboxNotFoundError extends SandboxError {}

/**
 * Raised when the sandbox object is missing expected metadata.
 */
export class SandboxMetadataError extends SandboxError {}

/**
 * Raised when an operation times out waiting for a sandbox resource.
 */
export class SandboxTimeoutError extends SandboxError {}

/**
 * Raised when the SandboxTemplate referenced by the WarmPool does not exist.
 */
export class SandboxTemplateNotFoundError extends SandboxError {}

/**
 * Raised when the referenced SandboxWarmPool does not exist.
 */
export class SandboxWarmPoolNotFoundError extends SandboxError {}

/**
 * Raised when the SandboxClaim reported a terminal Ready=False reason.
 *
 * The claim controller will not retry these (e.g. InvalidMetadata,
 * VolumeClaimTemplatesError, ClaimExpired); the claim cannot become ready
 * without user action, so ready-waits reject instead of waiting out the
 * timeout. See TERMINAL_CLAIM_READY_REASONS in constants.ts.
 */
export class SandboxClaimFailedError extends SandboxError {}

/**
 * Returns true if the error is a Kubernetes 404 (Not Found).
 * Handles both @kubernetes/client-node ApiException (.code / .statusCode).
 */
export function isK8s404(err: unknown): boolean {
  if (typeof err === "object" && err !== null) {
    const candidate = err as { code?: number; statusCode?: number };
    if (candidate.code === 404 || candidate.statusCode === 404) return true;
  }
  return false;
}

/**
 * Returns true if the error is a Kubernetes 409 (Conflict / AlreadyExists).
 * Handles both @kubernetes/client-node ApiException (.code / .statusCode).
 */
export function isK8s409(err: unknown): boolean {
  if (typeof err === "object" && err !== null) {
    const candidate = err as { code?: number; statusCode?: number };
    if (candidate.code === 409 || candidate.statusCode === 409) return true;
  }
  return false;
}
