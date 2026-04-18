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

import type { ExecutionResult, FileEntry } from "./types.js";
import {
  SandboxRequestError,
  SandboxResponseTooLargeError,
} from "./exceptions.js";

// --- bounded read helpers ---

/**
 * Reads response body chunks up to maxBytes.  Throws SandboxResponseTooLargeError
 * when the Content-Length header or the actual stream exceeds the limit.
 * Shared by readBoundedText and readBoundedBuffer to avoid code duplication.
 */
async function collectChunks(
  response: Response,
  maxBytes: number,
  operationDesc: string,
): Promise<Uint8Array[]> {
  const contentLength = response.headers.get("content-length");
  if (contentLength !== null && parseInt(contentLength, 10) > maxBytes) {
    await response.body?.cancel();
    throw new SandboxResponseTooLargeError(
      `Response too large for ${operationDesc}: content-length ` +
        `${contentLength} exceeds ${maxBytes} bytes`,
    );
  }

  const reader = response.body?.getReader();
  if (!reader) {
    // Fallback for environments without body streaming — use arrayBuffer() to
    // preserve raw bytes (response.text() would corrupt non-UTF-8 binary data).
    const arrayBuffer = await response.arrayBuffer();
    const bytes = new Uint8Array(arrayBuffer);
    if (bytes.byteLength > maxBytes) {
      throw new SandboxResponseTooLargeError(
        `Response too large for ${operationDesc}: ${bytes.byteLength} bytes exceeds ${maxBytes} bytes`,
      );
    }
    return [bytes];
  }

  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maxBytes) {
        await reader.cancel();
        throw new SandboxResponseTooLargeError(
          `Response too large for ${operationDesc}: exceeds ${maxBytes} bytes`,
        );
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  return chunks;
}

/**
 * Reads the response body as a UTF-8 string, enforcing a byte size limit.
 * Throws SandboxResponseTooLargeError if the body exceeds maxBytes.
 */
export async function readBoundedText(
  response: Response,
  maxBytes: number,
  operationDesc: string,
): Promise<string> {
  const chunks = await collectChunks(response, maxBytes, operationDesc);
  return Buffer.concat(chunks.map((c) => Buffer.from(c))).toString("utf-8");
}

/**
 * Reads the response body as a Buffer, enforcing a byte size limit.
 * Throws SandboxResponseTooLargeError if the body exceeds maxBytes.
 */
export async function readBoundedBuffer(
  response: Response,
  maxBytes: number,
  operationDesc: string,
): Promise<Buffer> {
  const chunks = await collectChunks(response, maxBytes, operationDesc);
  return Buffer.concat(chunks.map((c) => Buffer.from(c)));
}

/**
 * Reads an HTTP error-response body up to maxBytes and silently truncates
 * beyond it. Mirrors Go's io.LimitReader pattern (connector.go) — never
 * throws, so it never masks the original HTTPError being wrapped.
 * Returns "" on any read failure.
 */
export async function readBoundedErrorBody(
  response: Response,
  maxBytes: number,
): Promise<string> {
  try {
    const reader = response.body?.getReader();
    if (!reader) {
      const contentLength = response.headers.get("content-length");
      if (contentLength !== null && parseInt(contentLength, 10) > maxBytes) {
        return "";
      }
      const bytes = new Uint8Array(await response.arrayBuffer());
      return Buffer.from(bytes.subarray(0, maxBytes)).toString("utf-8");
    }
    const chunks: Uint8Array[] = [];
    let total = 0;
    try {
      while (total < maxBytes) {
        const { done, value } = await reader.read();
        if (done) break;
        const remaining = maxBytes - total;
        if (value.byteLength > remaining) {
          chunks.push(value.subarray(0, remaining));
          total += remaining;
          break;
        }
        chunks.push(value);
        total += value.byteLength;
      }
      await reader.cancel();
    } finally {
      reader.releaseLock();
    }
    return Buffer.concat(chunks.map((c) => Buffer.from(c))).toString("utf-8");
  } catch {
    return "";
  }
}

// --- response parsers ---

/**
 * Parses an unknown JSON value into an ExecutionResult.
 * Missing or wrong-typed fields fall back to safe defaults, matching Go behaviour.
 */
export function parseExecutionResult(data: unknown): ExecutionResult {
  if (typeof data !== "object" || data === null) {
    throw new SandboxRequestError(
      `Invalid execution result: expected object, got ${data === null ? "null" : typeof data}`,
      { operation: "execute" },
    );
  }
  const d = data as Record<string, unknown>;
  return {
    stdout: typeof d.stdout === "string" ? d.stdout : "",
    stderr: typeof d.stderr === "string" ? d.stderr : "",
    exitCode: typeof d.exit_code === "number" ? d.exit_code : -1,
  };
}

/**
 * Parses an unknown JSON value into a FileEntry array.
 * Entries with an unrecognised type are silently skipped, matching Go behaviour.
 */
export function parseFileEntries(data: unknown): FileEntry[] {
  if (!Array.isArray(data)) {
    throw new SandboxRequestError(
      `Invalid file listing: expected array, got ${data === null ? "null" : typeof data}`,
      { operation: "list" },
    );
  }
  const entries: FileEntry[] = [];
  for (const e of data) {
    if (typeof e !== "object" || e === null) continue;
    const entry = e as Record<string, unknown>;
    if (typeof entry.name !== "string") continue;
    const type = entry.type;
    if (type !== "file" && type !== "directory") continue;
    entries.push({
      name: entry.name,
      size: typeof entry.size === "number" ? entry.size : 0,
      type,
      modTime: typeof entry.mod_time === "number" ? entry.mod_time : 0,
    });
  }
  return entries;
}

/**
 * Parses an unknown JSON value into a boolean exists flag.
 * Returns false for missing or wrong-typed values.
 */
export function parseExistsResult(data: unknown): boolean {
  if (typeof data !== "object" || data === null) {
    throw new SandboxRequestError(
      `Invalid exists response: expected object, got ${data === null ? "null" : typeof data}`,
      { operation: "exists" },
    );
  }
  const d = data as Record<string, unknown>;
  return typeof d.exists === "boolean" ? d.exists : false;
}
