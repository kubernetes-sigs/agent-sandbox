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

import type { ExecutionResult, RunOptions } from "./types.js";

/**
 * The operation SandboxCommands delegates to — implemented by Sandbox and
 * injected at construction. Uses only public types.ts types; see files.ts
 * for why that matters for the public declaration surface.
 * @internal
 */
export interface CommandsOperations {
  run(
    command: string,
    args: readonly string[],
    opts?: RunOptions,
  ): Promise<ExecutionResult>;
}

/**
 * Public facade for `sandbox.commands.*`. Obtain an instance via
 * `sandbox.commands`; do not construct directly.
 */
export class SandboxCommands {
  /** @internal */
  constructor(private readonly ops: CommandsOperations) {}

  /**
   * Executes `command` with `args` as its argv (no shell involved) inside
   * the sandboxd container, runs it to completion, and returns its stdout,
   * stderr, and exit code. A non-zero exit code is a normal result, not a
   * thrown error; an executable that cannot be found is rejected by
   * sandboxd with a SandboxdRpcError (code "not_found"). For shell syntax
   * (pipes, redirects, `&&`, globbing), invoke a shell explicitly:
   * `run("sh", ["-c", "..."])`. Requires the optional `@bufbuild/protobuf`,
   * `@connectrpc/connect`, and `@connectrpc/connect-node` dependencies to be
   * installed.
   *
   * A failed call's underlying process may or may not have run to
   * completion — run() never retries automatically, since doing so could
   * re-execute a command that already had side effects.
   */
  run(
    command: string,
    args?: readonly string[],
    opts?: RunOptions,
  ): Promise<ExecutionResult>;
  run(command: string, opts?: RunOptions): Promise<ExecutionResult>;
  run(
    command: string,
    argsOrOpts?: readonly string[] | RunOptions,
    opts?: RunOptions,
  ): Promise<ExecutionResult> {
    if (Array.isArray(argsOrOpts)) {
      return this.ops.run(command, argsOrOpts, opts);
    }
    // `run(cmd, undefined, opts)` is valid under the (command, args, opts)
    // overload, so an omitted args must not drop the third argument.
    if (argsOrOpts === undefined) {
      return this.ops.run(command, [], opts);
    }
    return this.ops.run(command, [], argsOrOpts as RunOptions);
  }
}
