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
import type { Tracer } from "../trace-manager.js";
import { withSpan } from "../trace-manager.js";

export class Filesystem {
  private requestFn: RequestFn;
  private getTracer: () => Tracer | null;
  private traceServiceName: string;

  constructor(
    requestFn: RequestFn,
    getTracer: () => Tracer | null,
    traceServiceName: string,
  ) {
    this.requestFn = requestFn;
    this.getTracer = getTracer;
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

        const contentBytes: Uint8Array<ArrayBuffer> =
          typeof content === "string"
            ? new TextEncoder().encode(content)
            : new Uint8Array(
                content.buffer as ArrayBuffer,
                content.byteOffset,
                content.byteLength,
              );

        const filename = path.basename(filePath);
        const blob = new Blob([contentBytes]);
        const formData = new FormData();
        formData.append("file", blob, filename);

        await this.requestFn("POST", "upload", {
          body: formData,
          timeout,
        });

        console.info(`File '${filename}' uploaded successfully.`);
      },
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

        const encodedPath = encodeURIComponent(filePath);
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

        const encodedPath = encodeURIComponent(dirPath);
        const response = await this.requestFn("GET", `list/${encodedPath}`, {
          timeout,
        });

        const entries = (await response.json()) as Array<
          Record<string, unknown>
        >;

        if (!entries) {
          return [];
        }

        const fileEntries: FileEntry[] = entries.map((e) => ({
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

        const encodedPath = encodeURIComponent(filePath);
        const response = await this.requestFn("GET", `exists/${encodedPath}`, {
          timeout,
        });

        const data = (await response.json()) as Record<string, unknown>;
        const exists = (data.exists as boolean) ?? false;

        if (span.isRecording()) {
          span.setAttribute("sandbox.file.exists", exists);
        }

        return exists;
      },
    );
  }
}
