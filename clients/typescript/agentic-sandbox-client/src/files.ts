// Copyright 2026 The Kubernetes Authors.
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

import type {
  DeleteOptions,
  DirectoryListing,
  FileCallOptions,
  WriteOptions,
} from "./types.js";

/**
 * The operations SandboxFiles delegates to — implemented by Sandbox and
 * injected at construction. Every field here uses only public types.ts
 * types, so this file never needs to import anything from the connect layer
 * (tunnel.ts/rest.ts/process.ts) and can't leak those types into the public
 * declaration surface through SandboxFiles.
 * @internal
 */
export interface FilesOperations {
  read(path: string, opts?: FileCallOptions): Promise<Uint8Array>;
  write(
    path: string,
    content: string | Uint8Array,
    opts?: WriteOptions,
  ): Promise<void>;
  exists(path: string, opts?: FileCallOptions): Promise<boolean>;
  list(path: string, opts?: FileCallOptions): Promise<DirectoryListing>;
  delete(path: string, opts?: DeleteOptions): Promise<void>;
}

/**
 * Public facade for `sandbox.files.*`. All paths are sandbox-root-relative
 * POSIX paths — no `..` segments, no absolute paths — validated before any
 * network request. Obtain an instance via `sandbox.files`; do not construct
 * directly.
 */
export class SandboxFiles {
  /** @internal */
  constructor(private readonly ops: FilesOperations) {}

  /** Reads a file's full contents. Errors if `path` names a directory. */
  read(path: string, opts?: FileCallOptions): Promise<Uint8Array> {
    return this.ops.read(path, opts);
  }

  /**
   * Writes `content` to `path`, creating parent directories as needed.
   * A trailing `/` or the sandbox root itself is rejected — write() never
   * targets a directory.
   */
  write(
    path: string,
    content: string | Uint8Array,
    opts?: WriteOptions,
  ): Promise<void> {
    return this.ops.write(path, content, opts);
  }

  /** Returns whether `path` exists (file or directory). */
  exists(path: string, opts?: FileCallOptions): Promise<boolean> {
    return this.ops.exists(path, opts);
  }

  /** Lists a directory's immediate entries. Errors if `path` names a file. */
  list(path: string, opts?: FileCallOptions): Promise<DirectoryListing> {
    return this.ops.list(path, opts);
  }

  /**
   * Deletes a file, or a directory when `opts.recursive` is true. A trailing
   * `/` or the sandbox root itself is rejected — delete() never targets a
   * directory implicitly.
   */
  delete(path: string, opts?: DeleteOptions): Promise<void> {
    return this.ops.delete(path, opts);
  }
}
