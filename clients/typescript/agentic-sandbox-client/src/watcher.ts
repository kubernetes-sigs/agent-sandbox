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

import type { FileEvent, WatchOptions } from "./types.js";

/**
 * The operation SandboxWatcher delegates to — implemented by Sandbox and
 * injected at construction. Uses only public types.ts types; see files.ts
 * for why that matters for the public declaration surface.
 * @internal
 */
export interface WatcherOperations {
  watch(path: string, opts?: WatchOptions): AsyncIterableIterator<FileEvent>;
}

/**
 * Public facade for `sandbox.watcher.*`. Obtain an instance via
 * `sandbox.watcher`; do not construct directly.
 */
export class SandboxWatcher {
  /** @internal */
  constructor(private readonly ops: WatcherOperations) {}

  /**
   * Streams filesystem changes under the directory `path`, a
   * sandbox-root-relative POSIX path ("." is the sandbox root). The watch
   * opens when iteration starts and runs until the loop is left,
   * `opts.signal` is aborted, the Sandbox handle is closed, or sandboxd ends
   * the stream. Leaving a `for await` loop releases the watch on sandboxd.
   *
   * sandboxd reports a problem with a running watch as an `error` event
   * rather than a thrown error. When the problem ends the watch, such as the
   * watched directory being removed, that event is the last one. sandboxd
   * does not confirm that a watch is in place, so a change made right after
   * iteration starts can be missed.
   *
   * A watch that cannot start rejects on the first iteration with a
   * SandboxdRpcError, for example code "not_found" for a path that does not
   * exist or "unimplemented" for a runtime without the watcher service.
   * Invalid arguments throw a SandboxError before anything is sent. A failed
   * watch is never retried. Requires the same optional dependencies as
   * `sandbox.commands`.
   */
  watch(path: string, opts?: WatchOptions): AsyncIterableIterator<FileEvent> {
    return this.ops.watch(path, opts);
  }
}
