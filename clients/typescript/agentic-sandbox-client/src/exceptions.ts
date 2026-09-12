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
 * Options accepted by SandboxError and its subclasses. `telemetryCode` is a
 * fixed, low-cardinality string recorded on tracing spans (see
 * trace-manager.ts's withSpan error hook) — never a raw message, path, or
 * command. Subclasses that carry their own fixed classification (kind /
 * status code / rpc code) derive telemetryCode from it automatically.
 */
export interface SandboxErrorOptions extends ErrorOptions {
  telemetryCode?: string;
}

/**
 * Base class for all sandbox-related errors.
 */
export class SandboxError extends Error {
  readonly telemetryCode: string;

  constructor(message: string, options?: SandboxErrorOptions) {
    super(message, options);
    this.name = this.constructor.name;
    this.telemetryCode = options?.telemetryCode ?? "unknown";
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
 * Raised when an operation times out waiting for a sandbox resource, a
 * shared sandboxd connect, or an individual files/run call.
 */
export class SandboxTimeoutError extends SandboxError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, { ...options, telemetryCode: "timeout" });
  }
}

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
 * Raised when a files/run operation is rejected because the Sandbox handle is
 * closing or closed (see Sandbox.close() / closeLocal()).
 */
export class SandboxClosedError extends SandboxError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, { ...options, telemetryCode: "closed" });
  }
}

/**
 * Fixed, low-cardinality classification of a sandboxd connection failure.
 * Never a free-form string: onFactory/tunnel code must map any underlying
 * cause into one of these before constructing SandboxConnectionError.
 */
export type SandboxConnectionErrorKind =
  | "handshake"
  | "port_forward"
  | "protocol"
  | "socket"
  | "listener"
  | "unavailable";

/**
 * Raised when the shared sandboxd connection (port-forward tunnel or gRPC
 * transport) fails outside of a timeout or explicit cancellation. `detail` is
 * a truncated, sanitized diagnostic (see ERROR_DETAIL_MAX_BYTES in
 * constants.ts) — never the raw server/error-channel text.
 */
export class SandboxConnectionError extends SandboxError {
  readonly kind: SandboxConnectionErrorKind;
  readonly detail?: string;

  constructor(
    message: string,
    kind: SandboxConnectionErrorKind,
    options?: ErrorOptions & { detail?: string },
  ) {
    super(message, { cause: options?.cause, telemetryCode: kind });
    this.kind = kind;
    this.detail = options?.detail;
  }
}

/**
 * REST API error codes, normalized from sandboxd's HTTP status/body into a
 * small fixed enum. Never a free-form string.
 */
export type SandboxdApiCode =
  | "NOT_FOUND"
  | "PERMISSION_DENIED"
  | "CONFLICT"
  | "INTERNAL"
  | "UNKNOWN";

/**
 * Raised for a non-2xx response from the sandboxd REST files API. `detail` is
 * a truncated preview of the server's error body (see ERROR_DETAIL_MAX_BYTES).
 */
export class SandboxdApiError extends SandboxError {
  readonly status: number;
  readonly code: SandboxdApiCode;
  readonly detail?: string;

  constructor(
    message: string,
    status: number,
    code: SandboxdApiCode,
    options?: ErrorOptions & { detail?: string },
  ) {
    super(message, {
      cause: options?.cause,
      telemetryCode: code.toLowerCase(),
    });
    this.status = status;
    this.code = code;
    this.detail = options?.detail;
  }
}

/**
 * Raised for a gRPC status other than OK/DEADLINE_EXCEEDED/UNAVAILABLE from
 * the sandboxd ProcessService. `code` is Connect's numeric status mapped to
 * its known snake_case name (e.g. "resource_exhausted"), or "unknown" for an
 * unrecognized code — never the raw rawMessage text, which goes in `detail`.
 */
export class SandboxdRpcError extends SandboxError {
  readonly code: string;
  readonly detail?: string;

  constructor(
    message: string,
    code: string,
    options?: ErrorOptions & { detail?: string },
  ) {
    super(message, { cause: options?.cause, telemetryCode: code });
    this.code = code;
    this.detail = options?.detail;
  }
}

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
