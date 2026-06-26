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

import * as path from "node:path";
import {
  MAX_DOWNLOAD_SIZE,
  MAX_ERROR_BODY_BYTES,
  MAX_METADATA_RESPONSE_SIZE,
  MAX_UPLOAD_SIZE,
} from "../constants.js";
import {
  parseExistsResult,
  parseFileEntries,
  readBoundedBuffer,
  readBoundedText,
} from "../response-utils.js";
import type { CallOptions, FileEntry, RequestFn } from "../types.js";

function normalizeCallOptions(
  arg: number | CallOptions | undefined,
  defaultTimeoutSec: number,
): { timeout: number; signal?: AbortSignal; allowUnsafePaths: boolean } {
  if (typeof arg === "number") {
    return { timeout: arg, allowUnsafePaths: false };
  }
  if (arg == null) {
    return { timeout: defaultTimeoutSec, allowUnsafePaths: false };
  }
  return {
    timeout: arg.timeout ?? defaultTimeoutSec,
    signal: arg.signal,
    allowUnsafePaths: arg.allowUnsafePaths ?? false,
  };
}

/**
 * Percent-encodes a file path segment so that every character not in the
 * RFC 3986 unreserved set is escaped.  encodeURIComponent() leaves
 * ! ' ( ) * unescaped; this function encodes those as well, matching the
 * behaviour of the Go and Python clients.
 */
function encodePathSegment(s: string): string {
  return encodeURIComponent(s).replace(
    /[!'()*]/g,
    (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`,
  );
}

/**
 * path.posix.normalize preserves embedded NULs; a NUL in the filename
 * truncates at the C/syscall layer, so "foo\x00../etc/passwd" would survive
 * the ".." split yet resolve unexpectedly on the server.
 *
 * path.posix.normalize treats '\' as a literal character (legal on Linux),
 * so '..\\etc\\passwd' splits as ['..\\etc\\passwd'] with no '..' component —
 * the traversal check would be silently bypassed on Windows-originated input.
 */
function validatePathSafety(filePath: string, label: string): void {
  for (let i = 0; i < filePath.length; i++) {
    const code = filePath.charCodeAt(i);
    if (code < 0x20 || code === 0x7f) {
      throw new Error(
        `${label} contains ASCII control characters: ${JSON.stringify(filePath)}`,
      );
    }
  }

  if (filePath.includes("\\")) {
    throw new Error(
      `${label} must use forward slashes: ${JSON.stringify(filePath)}`,
    );
  }

  if (filePath !== filePath.trim()) {
    throw new Error(
      `${label} has leading or trailing whitespace: ${JSON.stringify(filePath)}`,
    );
  }
}

function safeUploadPath(filePath: string): string {
  validatePathSafety(filePath, "Upload path");

  const normalized = path.posix.normalize(filePath).replace(/^\/+/, "");
  if (!normalized || normalized === ".") {
    throw new Error(`Upload path '${filePath}' does not name a file.`);
  }

  if (normalized.split("/").some((part) => part === "..")) {
    throw new Error(`Upload path '${filePath}' escapes the sandbox root.`);
  }

  return normalized;
}

function safeDirPath(dirPath: string): string {
  validatePathSafety(dirPath, "Directory path");

  const normalized = path.posix.normalize(dirPath).replace(/^\/+/, "") || ".";

  if (normalized.split("/").some((part) => part === "..")) {
    throw new Error(`Directory path '${dirPath}' escapes the sandbox root.`);
  }

  return normalized;
}

import { SandboxRequestError } from "../exceptions.js";
import type { Tracer } from "../trace-manager.js";
import { withSpan } from "../trace-manager.js";

export class Filesystem {
  private requestFn: RequestFn;
  private getTracer: () => Tracer | null;
  private getParentContext: () => unknown;
  private traceServiceName: string;

  constructor(
    requestFn: RequestFn,
    getTracer: () => Tracer | null,
    traceServiceName: string,
    getParentContext: () => unknown = () => null,
  ) {
    this.requestFn = requestFn;
    this.getTracer = getTracer;
    this.getParentContext = getParentContext;
    this.traceServiceName = traceServiceName;
  }

  async write(
    filePath: string,
    content: Buffer | string,
    options?: number | CallOptions,
  ): Promise<void> {
    const { timeout, signal, allowUnsafePaths } = normalizeCallOptions(
      options,
      60,
    );
    await withSpan(
      this.getTracer(),
      this.traceServiceName,
      "write",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
          span.setAttribute("sandbox.file.size", Buffer.byteLength(content));
        }

        if (!filePath?.trim()) {
          throw new Error("write: file path cannot be empty");
        }

        const safePath = allowUnsafePaths ? filePath : safeUploadPath(filePath);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", safePath);
        }

        const contentBytes: Uint8Array<ArrayBuffer> =
          typeof content === "string"
            ? new TextEncoder().encode(content)
            : new Uint8Array(
                content.buffer as ArrayBuffer,
                content.byteOffset,
                content.byteLength,
              );

        if (contentBytes.byteLength > MAX_UPLOAD_SIZE) {
          throw new SandboxRequestError(
            `File too large: ${contentBytes.byteLength} bytes exceeds upload limit of ${MAX_UPLOAD_SIZE} bytes`,
            { operation: "POST upload" },
          );
        }

        const blob = new Blob([contentBytes]);
        const formData = new FormData();
        formData.append("file", blob, safePath);

        await this.requestFn("POST", "upload", {
          body: formData,
          timeout,
          signal,
          maxRetries: 1, // file upload is non-idempotent; never retry
        });

        console.info(`File '${safePath}' uploaded successfully.`);
      },
      this.getParentContext(),
    );
  }

  async read(
    filePath: string,
    options?: number | CallOptions,
  ): Promise<Buffer> {
    const { timeout, signal, allowUnsafePaths } = normalizeCallOptions(
      options,
      60,
    );
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "read",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
        }

        if (!filePath?.trim()) {
          throw new Error("read: file path cannot be empty");
        }

        const safePath = allowUnsafePaths ? filePath : safeUploadPath(filePath);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", safePath);
        }

        const encodedPath = encodePathSegment(safePath);
        const response = await this.requestFn(
          "GET",
          `download/${encodedPath}`,
          {
            timeout,
            signal,
          },
        );

        const buffer = await readBoundedBuffer(
          response,
          MAX_DOWNLOAD_SIZE,
          "download",
        );

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.size", buffer.length);
        }

        return buffer;
      },
      this.getParentContext(),
    );
  }

  async list(
    dirPath: string,
    options?: number | CallOptions,
  ): Promise<FileEntry[]> {
    const { timeout, signal, allowUnsafePaths } = normalizeCallOptions(
      options,
      60,
    );
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "list",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", dirPath);
        }

        if (!dirPath?.trim()) {
          throw new Error("list: directory path cannot be empty");
        }

        const safePath = allowUnsafePaths ? dirPath : safeDirPath(dirPath);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", safePath);
        }

        const encodedPath = encodePathSegment(safePath);
        const response = await this.requestFn("GET", `list/${encodedPath}`, {
          timeout,
          signal,
        });

        const rawText = await readBoundedText(
          response,
          MAX_METADATA_RESPONSE_SIZE,
          "list",
        );
        let data: unknown;
        try {
          data = JSON.parse(rawText);
        } catch (err) {
          const preview =
            rawText.length > MAX_ERROR_BODY_BYTES
              ? `${[...rawText].slice(0, MAX_ERROR_BODY_BYTES).join("")}…`
              : rawText;
          throw new SandboxRequestError(
            `Failed to decode JSON response from sandbox: ${preview}`,
            { cause: err, operation: "list" },
          );
        }
        const fileEntries = parseFileEntries(data);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.count", fileEntries.length);
        }

        return fileEntries;
      },
      this.getParentContext(),
    );
  }

  async exists(
    filePath: string,
    options?: number | CallOptions,
  ): Promise<boolean> {
    const { timeout, signal, allowUnsafePaths } = normalizeCallOptions(
      options,
      60,
    );
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "exists",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
        }

        if (!filePath?.trim()) {
          throw new Error("exists: file path cannot be empty");
        }

        // safeDirPath is used because exists() applies to both files and
        // directories; "." (sandbox root) is a valid target.
        const safePath = allowUnsafePaths ? filePath : safeDirPath(filePath);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", safePath);
        }

        const encodedPath = encodePathSegment(safePath);
        const response = await this.requestFn("GET", `exists/${encodedPath}`, {
          timeout,
          signal,
        });

        const rawText = await readBoundedText(
          response,
          MAX_METADATA_RESPONSE_SIZE,
          "exists",
        );
        let data: unknown;
        try {
          data = JSON.parse(rawText);
        } catch (err) {
          const preview =
            rawText.length > MAX_ERROR_BODY_BYTES
              ? `${[...rawText].slice(0, MAX_ERROR_BODY_BYTES).join("")}…`
              : rawText;
          throw new SandboxRequestError(
            `Failed to decode JSON response from sandbox: ${preview}`,
            { cause: err, operation: "exists" },
          );
        }
        const exists = parseExistsResult(data);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.exists", exists);
        }

        return exists;
      },
      this.getParentContext(),
    );
  }
}
