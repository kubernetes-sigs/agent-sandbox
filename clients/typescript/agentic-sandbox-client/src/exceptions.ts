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
 * Raised when the sandbox is not ready for communication.
 */
export class SandboxNotReadyError extends SandboxError {}

/**
 * Raised when the sandbox or sandbox claim cannot be found or was deleted.
 */
export class SandboxNotFoundError extends SandboxError {}

/**
 * Raised when the port-forward process crashes.
 */
export class SandboxPortForwardError extends SandboxError {}

/**
 * Raised when the sandbox object is missing expected metadata.
 */
export class SandboxMetadataError extends SandboxError {}

/**
 * Raised when an operation times out waiting for a sandbox resource.
 */
export class SandboxTimeoutError extends SandboxError {}

/**
 * Raised when an HTTP request to the sandbox fails.
 */
export class SandboxRequestError extends SandboxError {
  readonly statusCode: number | undefined;
  readonly response: Response | undefined;
  readonly body: string | undefined;
  readonly operation: string | undefined;

  constructor(
    message: string,
    options?: ErrorOptions & {
      statusCode?: number;
      response?: Response;
      body?: string;
      operation?: string;
    },
  ) {
    super(message, options);
    this.statusCode = options?.statusCode;
    this.response = options?.response;
    this.body = options?.body;
    this.operation = options?.operation;
  }
}

/**
 * Raised when a response body exceeds the configured size limit.
 */
export class SandboxResponseTooLargeError extends SandboxRequestError {}

/**
 * Returns true if the error is a Kubernetes 404 (Not Found).
 * Handles both @kubernetes/client-node ApiException (.code) and
 * older-style errors that include "404" in the message.
 */
export function isK8s404(err: unknown): boolean {
  if (typeof err === "object" && err !== null) {
    // @kubernetes/client-node ApiException has a numeric .code property
    if ((err as { code?: number }).code === 404) return true;
  }
  return err instanceof Error && err.message.includes("404");
}
