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

import type { FileEntry, RequestFn } from "../types.js";

/**
 * Percent-encodes a file path segment so that every character not in the
 * RFC 3986 unreserved set is escaped.  encodeURIComponent() leaves
 * ! ' ( ) * unescaped; this function encodes those as well, matching the
 * behaviour of the Go and Python clients.
 */
function encodePathSegment(s: string): string {
  return encodeURIComponent(s).replace(
    /[!'()*]/g,
    (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase(),
  );
}
import type { Tracer } from "../trace-manager.js";
import { withSpan } from "../trace-manager.js";
import { SandboxRequestError } from "../exceptions.js";

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
    timeout: number = 60,
  ): Promise<void> {
    await withSpan(
      this.getTracer(),
      this.traceServiceName,
      "write",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
          span.setAttribute("sandbox.file.size", content.length);
        }

        if (!filePath) {
          throw new Error("write: file path cannot be empty");
        }

        const base = path.basename(filePath);
        if (
          base === "." ||
          base === ".." ||
          base === "/" ||
          base !== filePath
        ) {
          throw new Error(
            `write: "${filePath}" is not a plain filename (resolved to "${base}"); ` +
              `pass only the filename, not a path with directories`,
          );
        }

        const contentBytes: Uint8Array<ArrayBuffer> =
          typeof content === "string"
            ? new TextEncoder().encode(content)
            : new Uint8Array(
                content.buffer as ArrayBuffer,
                content.byteOffset,
                content.byteLength,
              );

        const blob = new Blob([contentBytes]);
        const formData = new FormData();
        formData.append("file", blob, base);

        await this.requestFn("POST", "upload", {
          body: formData,
          timeout,
          maxRetries: 1, // file upload is non-idempotent; never retry
        });

        console.info(`File '${base}' uploaded successfully.`);
      },
      this.getParentContext(),
    );
  }

  async read(filePath: string, timeout: number = 60): Promise<Buffer> {
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "read",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
        }

        const encodedPath = encodePathSegment(filePath);
        const response = await this.requestFn(
          "GET",
          `download/${encodedPath}`,
          {
            timeout,
          },
        );

        const arrayBuffer = await response.arrayBuffer();
        const buffer = Buffer.from(arrayBuffer);

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.size", buffer.length);
        }

        return buffer;
      },
      this.getParentContext(),
    );
  }

  async list(dirPath: string, timeout: number = 60): Promise<FileEntry[]> {
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "list",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", dirPath);
        }

        const encodedPath = encodePathSegment(dirPath);
        const response = await this.requestFn("GET", `list/${encodedPath}`, {
          timeout,
        });

        const rawText = await response.text();
        let entries: unknown;
        try {
          entries = JSON.parse(rawText);
        } catch (err) {
          throw new SandboxRequestError(
            `Failed to decode JSON response from sandbox: ${rawText}`,
            { cause: err },
          );
        }

        if (!Array.isArray(entries)) {
          return [];
        }

        const fileEntries: FileEntry[] = (
          entries as Array<Record<string, unknown>>
        ).map((e) => ({
          name: e.name as string,
          size: e.size as number,
          type: e.type as "file" | "directory",
          modTime: e.mod_time as number,
        }));

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.count", fileEntries.length);
        }

        return fileEntries;
      },
      this.getParentContext(),
    );
  }

  async exists(filePath: string, timeout: number = 60): Promise<boolean> {
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "exists",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.file.path", filePath);
        }

        const encodedPath = encodePathSegment(filePath);
        const response = await this.requestFn("GET", `exists/${encodedPath}`, {
          timeout,
        });

        const rawText = await response.text();
        let data: Record<string, unknown>;
        try {
          data = JSON.parse(rawText) as Record<string, unknown>;
        } catch (err) {
          throw new SandboxRequestError(
            `Failed to decode JSON response from sandbox: ${rawText}`,
            { cause: err },
          );
        }
        const exists = (data.exists as boolean) ?? false;

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.exists", exists);
        }

        return exists;
      },
      this.getParentContext(),
    );
  }
}
