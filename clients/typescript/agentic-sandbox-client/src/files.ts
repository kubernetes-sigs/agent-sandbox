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
  readStream(
    path: string,
    opts?: FileCallOptions,
  ): Promise<ReadableStream<Uint8Array>>;
  writeStream(
    path: string,
    content: ReadableStream<Uint8Array>,
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

  /**
   * Reads a file's full contents into memory, bounded by
   * `sandboxd.maxDownloadSize`. The SDK never retries automatically. You can
   * call it again to retrieve the file from the beginning; the file may have
   * changed between attempts. Use `readStream()` to avoid buffering large
   * files. Errors if `path` names a directory.
   */
  read(path: string, opts?: FileCallOptions): Promise<Uint8Array> {
    return this.ops.read(path, opts);
  }

  /**
   * Writes `content` to `path`, creating parent directories as needed. The
   * whole payload is held in memory and bounded by `sandboxd.maxUploadSize`.
   * The SDK never retries automatically. The same unchanged content can be
   * sent again. sandboxd replaces the target atomically using a temporary
   * file and rename, but a failed call may already have committed the write
   * if the acknowledgement was lost. Retrying may overwrite concurrent
   * updates. Use `writeStream()` to avoid buffering large payloads.
   *
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

  /**
   * Opens a file for streaming download and resolves once the response
   * headers have been validated (HTTP 200 with a file body; a directory is
   * rejected like `read()`). You MUST read the returned stream to completion
   * or cancel it; when using a reader, call `reader.cancel()`. The operation
   * counts as in-flight until completion, cancellation, or failure, and
   * `close()` waits up to its cleanup timeout before teardown. `opts.timeoutMs`
   * covers connection setup and consumption, and keeps running even when you
   * stop reading. Timeout errors the stream and releases the operation
   * without waiting for another read. Received bytes are bounded by
   * `sandboxd.maxDownloadSize`; exceeding it errors the stream. To retry,
   * discard the partial output, reset your destination, and call
   * `readStream()` again from the beginning. The file may have changed
   * between attempts. Resuming a partial download and automatic retry are
   * not supported.
   */
  readStream(
    path: string,
    opts?: FileCallOptions,
  ): Promise<ReadableStream<Uint8Array>> {
    return this.ops.readStream(path, opts);
  }

  /**
   * Uploads `content` in a single request without buffering the whole
   * payload. The input is consumed at most once; to retry, provide a fresh
   * stream producing the same content. sandboxd replaces the target
   * atomically using a temporary file and rename. A failed call may already
   * have committed the write if the acknowledgement was lost, and retrying
   * may overwrite concurrent updates. `sandboxd.maxUploadSize` is enforced
   * before forwarding each chunk; the server may receive a prefix within the
   * limit before an oversized upload is aborted. `opts.timeoutMs` covers
   * connection setup until the server acknowledges the write, including time
   * waiting for input. Once consumption starts, the SDK cancels the source on
   * early termination; validation failures leave it with the caller. A
   * locked stream is rejected before the upload request. No automatic retry
   * is performed.
   */
  writeStream(
    path: string,
    content: ReadableStream<Uint8Array>,
    opts?: WriteOptions,
  ): Promise<void> {
    return this.ops.writeStream(path, content, opts);
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
