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

import {
  DEFAULT_OPERATION_TIMEOUT_MS,
  STDIN_CHUNK_BYTES,
} from "./constants.js";
import {
  SandboxClosedError,
  SandboxError,
  SandboxTimeoutError,
} from "./exceptions.js";
import type {
  ExecutionResult,
  ProcessCallOptions,
  ProcessEvent,
  ProcessSignal,
  PtySize,
  RunOptions,
  StartOptions,
} from "./types.js";

/**
 * The operations SandboxCommands delegates to — implemented by Sandbox and
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
  start(
    command: string,
    args: readonly string[],
    opts?: StartOptions,
  ): Promise<ProcessHandle>;
}

/**
 * The connection-bound half of a started process, implemented by Sandbox
 * and injected into ProcessHandle. Every failure it throws is already
 * classified into the public error types, and `next()` never reports "done"
 * before an exit event was returned.
 * @internal
 */
export interface ProcessSession {
  readonly pid: number;
  readonly pty: boolean;
  next(): Promise<IteratorResult<ProcessEvent, undefined>>;
  /** `null` closes stdin (EOF). */
  writeStdin(data: Uint8Array | null, opts?: ProcessCallOptions): Promise<void>;
  signal(sig: ProcessSignal, opts?: ProcessCallOptions): Promise<void>;
  resize(size: PtySize, opts?: ProcessCallOptions): Promise<void>;
  /** Tears the stream down; sandboxd then SIGKILLs the process group. */
  cancel(): void;
  /**
   * Calls `cb` once, with the already-classified failure, as soon as the
   * stream is torn down (caller abort, handle or Sandbox close, lost
   * connection) — whether or not anyone is reading `next()`. Returns an
   * unsubscribe function.
   */
  onAbort(cb: (err: unknown) => void): () => void;
}

const SIGNALS: ReadonlySet<string> = new Set(["SIGINT", "SIGTERM", "SIGKILL"]);

function invalidArgument(message: string): SandboxError {
  return new SandboxError(message, { telemetryCode: "invalid_argument" });
}

/**
 * Validates and defaults a per-call `timeoutMs`. Lives here rather than in
 * sandbox.ts because ProcessHandle.write() must validate it before splitting
 * its budget across chunks, and sandbox.ts already imports this module.
 * @internal
 */
export function validateTimeoutMs(
  name: string,
  value: number | undefined,
): number {
  if (value === undefined) return DEFAULT_OPERATION_TIMEOUT_MS;
  if (!Number.isInteger(value) || value <= 0 || value > 2147483647) {
    throw invalidArgument(
      `${name} must be a positive integer <= 2147483647, got: ${value}`,
    );
  }
  return value;
}

/** Validates a terminal size. Shared with sandbox.commands.start(). @internal */
export function validatePtySize(size: unknown, name: string): PtySize {
  const { cols, rows } = (size ?? {}) as Partial<PtySize>;
  for (const [field, value] of [
    ["cols", cols],
    ["rows", rows],
  ] as const) {
    if (
      typeof value !== "number" ||
      !Number.isInteger(value) ||
      value < 1 ||
      value > 65535
    ) {
      throw invalidArgument(
        `${name}.${field} must be an integer in [1, 65535]`,
      );
    }
  }
  return { cols: cols as number, rows: rows as number };
}

/**
 * A process started with `sandbox.commands.start()`. Receive its output
 * either by passing `onStdout`/`onStderr` to `start()`, by iterating
 * `events`, or by just calling `wait()` (which discards it); these are
 * mutually exclusive, except that `wait()` takes over from an `events`
 * loop that has been left. Nothing is buffered on the SDK side: until one
 * of them is in use the stream is not read, and sandboxd's process blocks
 * on a full pipe.
 *
 * The process lives exactly as long as its stream. Closing the handle
 * (`close()`), aborting `StartOptions.signal`, or closing the Sandbox
 * handle tears the stream down, and sandboxd then SIGKILLs the process's
 * whole process group. There is no way to detach and leave it running.
 * `pid` is sandboxd's own numbering, meaningful only for the connection the
 * process was started on.
 */
export class ProcessHandle {
  readonly pid: number;

  private consumer: "none" | "callbacks" | "iterator" | "drain" = "none";
  private closed = false;
  private terminal = false;
  private exitSeen = false;
  private streamEnded = false;
  /** Set by the iterator's `return()`, i.e. when a `for await` loop is left. */
  private iteratorReleased = false;
  private pullChain: Promise<unknown> = Promise.resolve();
  private readonly exit: Promise<{ exitCode: number }>;
  private resolveExit!: (result: { exitCode: number }) => void;
  private rejectExit!: (err: unknown) => void;
  private iterator?: AsyncIterableIterator<ProcessEvent>;
  private unwatch?: () => void;

  /** @internal */
  constructor(
    private readonly session: ProcessSession,
    private readonly callbacks: Pick<StartOptions, "onStdout" | "onStderr">,
  ) {
    this.pid = session.pid;
    this.exit = new Promise((resolve, reject) => {
      this.resolveExit = resolve;
      this.rejectExit = reject;
    });
    // A caller that only iterates `events` never awaits this.
    this.exit.catch(() => {});
    // Reads are what normally surface a failure, but nothing reads once the
    // caller stops iterating, so cancellation must settle `exit` on its own.
    this.unwatch = session.onAbort((err) => this.fail(err));
    if (callbacks.onStdout || callbacks.onStderr) {
      this.consumer = "callbacks";
      void this.pump();
    }
  }

  /**
   * The process's events in order, ending with one "exit" event. Only
   * available if no callbacks were given to `start()` and `wait()` has not
   * taken over. Iterating is resumable: leaving a `for await` loop early
   * neither kills the process nor discards anything, until `wait()` is
   * called, which then discards the rest of the output.
   */
  get events(): AsyncIterable<ProcessEvent> {
    if (this.consumer !== "none" && this.consumer !== "iterator") {
      throw invalidArgument(
        "events is unavailable: output is already consumed by start() callbacks or wait()",
      );
    }
    this.consumer = "iterator";
    if (!this.iterator) {
      const iterator: AsyncIterableIterator<ProcessEvent> = {
        [Symbol.asyncIterator]: () => iterator,
        next: async () => {
          if (this.consumer === "drain") {
            throw invalidArgument(
              "events is unavailable: wait() took over consuming the output",
            );
          }
          this.iteratorReleased = false;
          const step = await this.pull();
          return step.done ? { done: true, value: undefined } : step;
        },
        // Called when a `for await` loop is left early. The process keeps
        // running and nothing is discarded, but the stream is no longer read,
        // so a later wait() has to read it.
        return: async () => {
          this.iteratorReleased = true;
          return { done: true, value: undefined };
        },
      };
      this.iterator = iterator;
    }
    return this.iterator;
  }

  /**
   * Resolves with the exit code once the process exits (-1 if it was killed
   * by a signal). If neither callbacks nor `events` are in use, or an
   * `events` loop has been left (break, return, or throw), this starts
   * discarding the remaining output so the process can make progress;
   * `events` is unavailable from then on. An `events` iterator driven by
   * hand with `next()` must be finished with `return()` for this to apply.
   * Rejects as soon
   * as the stream is torn down (abort, close, lost connection), even if
   * output is no longer being read.
   */
  wait(): Promise<{ exitCode: number }> {
    // Only a released iterator is taken over: while a loop is still running
    // (even if its body is awaiting something), its events must not be
    // stolen.
    if (
      this.consumer === "none" ||
      (this.consumer === "iterator" && this.iteratorReleased)
    ) {
      this.consumer = "drain";
      void this.pump();
    }
    return this.exit;
  }

  /**
   * Sends bytes to the process's stdin (for a PTY, its terminal input).
   * Large payloads are sent as sequential chunks that share one
   * `opts.timeoutMs` budget for the whole call; if a call fails, an unknown
   * prefix has already been delivered.
   */
  async write(
    data: string | Uint8Array,
    opts?: ProcessCallOptions,
  ): Promise<void> {
    this.assertOpen();
    const bytes =
      typeof data === "string" ? new TextEncoder().encode(data) : data;
    if (!(bytes instanceof Uint8Array)) {
      throw invalidArgument("data must be a string or Uint8Array");
    }
    // Each chunk is its own RPC with its own timer, so the budget is fixed
    // once here and every chunk gets only what is left of it. It is
    // validated first: a non-positive budget would otherwise surface as a
    // timeout instead of invalid_argument.
    const budgetMs = validateTimeoutMs("timeoutMs", opts?.timeoutMs);
    const deadline = Date.now() + budgetMs;
    for (let i = 0; i < bytes.length; i += STDIN_CHUNK_BYTES) {
      const remainingMs = deadline - Date.now();
      if (remainingMs <= 0) {
        throw new SandboxTimeoutError(`write timed out after ${budgetMs}ms`);
      }
      await this.session.writeStdin(bytes.subarray(i, i + STDIN_CHUNK_BYTES), {
        ...opts,
        timeoutMs: Math.ceil(remainingMs),
      });
    }
  }

  /**
   * Closes the process's stdin (EOF). Rejected for a PTY process: sandboxd
   * would close the terminal itself, not just its input. Send `"\x04"`
   * (Ctrl-D) with `write()` instead.
   */
  async closeStdin(opts?: ProcessCallOptions): Promise<void> {
    this.assertOpen();
    if (this.session.pty) {
      throw invalidArgument(
        'closeStdin() is not supported for a PTY process; write("\\x04") instead',
      );
    }
    await this.session.writeStdin(null, opts);
  }

  /** Delivers `sig` to the process's whole process group. */
  async signal(sig: ProcessSignal, opts?: ProcessCallOptions): Promise<void> {
    this.assertOpen();
    if (!SIGNALS.has(sig)) {
      throw invalidArgument('signal must be "SIGINT", "SIGTERM", or "SIGKILL"');
    }
    await this.session.signal(sig, opts);
  }

  /** Same as `signal("SIGKILL")`. */
  kill(opts?: ProcessCallOptions): Promise<void> {
    return this.signal("SIGKILL", opts);
  }

  /** Resizes the terminal. Rejected by sandboxd for a non-PTY process. */
  async resize(size: PtySize, opts?: ProcessCallOptions): Promise<void> {
    this.assertOpen();
    await this.session.resize(validatePtySize(size, "size"), opts);
  }

  /**
   * Tears the stream down, which makes sandboxd SIGKILL the process group
   * (a no-op for a process that already exited). Idempotent. Pending and
   * later reads of `events`/`wait()` that have not seen the exit reject with
   * SandboxClosedError.
   */
  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.session.cancel();
    this.fail(new SandboxClosedError("Process handle is closed"));
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.close();
  }

  private assertOpen(): void {
    if (this.closed) throw new SandboxClosedError("Process handle is closed");
  }

  private fail(err: unknown): void {
    if (this.terminal) return;
    this.markTerminal();
    this.rejectExit(err);
  }

  private markTerminal(): void {
    this.terminal = true;
    this.unwatch?.();
  }

  /**
   * Reads one event. Reads are serialized, so a hand-driven `next()` still
   * in flight when wait() takes over cannot race the drain.
   */
  private pull(): Promise<IteratorResult<ProcessEvent, undefined>> {
    const step = this.pullChain.then(() => this.pullOnce());
    this.pullChain = step.catch(() => {});
    return step;
  }

  /** Reads one event, settling `exit` on the terminal outcomes. */
  private async pullOnce(): Promise<IteratorResult<ProcessEvent, undefined>> {
    if (this.streamEnded || (this.terminal && !this.exitSeen)) {
      // Already finished: a normal end, or the failure recorded in `exit`.
      await this.exit;
      return { done: true, value: undefined };
    }
    try {
      const step = await this.session.next();
      if (step.done) {
        this.streamEnded = true;
        return step;
      }
      if (step.value.type === "exit") {
        this.exitSeen = true;
        this.markTerminal();
        this.resolveExit({ exitCode: step.value.exitCode });
      }
      return step;
    } catch (err) {
      if (this.exitSeen) {
        // A failure while draining the stream's tail, after a clean exit,
        // changes nothing the caller was promised.
        this.streamEnded = true;
        return { done: true, value: undefined };
      }
      const failure = this.closed
        ? new SandboxClosedError("Process handle is closed")
        : err;
      this.fail(failure);
      throw failure;
    }
  }

  private async pump(): Promise<void> {
    try {
      while (true) {
        const step = await this.pull();
        if (step.done) return;
        const event = step.value;
        if (event.type === "stdout") this.callbacks.onStdout?.(event.data);
        else if (event.type === "stderr") this.callbacks.onStderr?.(event.data);
      }
    } catch (err) {
      // pull() settles `exit` itself for a stream failure, so reaching here
      // with it still pending means a user callback threw.
      if (!this.terminal) {
        this.closed = true;
        // Settle with the callback's error first: cancelling below also
        // reports a "closed" failure through onAbort().
        this.fail(err);
        this.session.cancel();
      }
    }
  }
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

  /**
   * Starts `command` with `args` as its argv (no shell involved) and
   * resolves once sandboxd reports it running, with a {@link ProcessHandle}
   * to read its output, feed it stdin, signal it, and (with `opts.pty`)
   * resize its terminal. Use this instead of `run()` for long-running or
   * interactive processes. `opts.timeoutMs` bounds only the time until the
   * process has started, not its lifetime.
   *
   * The process is tied to its stream: closing the handle, aborting
   * `opts.signal`, or closing the Sandbox handle kills it (SIGKILL to its
   * process group). Like `run()`, an executable that cannot be found is
   * rejected with a SandboxdRpcError (code "not_found"), and a failed call
   * is never retried. Requires the same optional dependencies as `run()`.
   */
  start(
    command: string,
    args?: readonly string[],
    opts?: StartOptions,
  ): Promise<ProcessHandle>;
  start(command: string, opts?: StartOptions): Promise<ProcessHandle>;
  start(
    command: string,
    argsOrOpts?: readonly string[] | StartOptions,
    opts?: StartOptions,
  ): Promise<ProcessHandle> {
    if (Array.isArray(argsOrOpts)) {
      return this.ops.start(command, argsOrOpts, opts);
    }
    if (argsOrOpts === undefined) {
      return this.ops.start(command, [], opts);
    }
    return this.ops.start(command, [], argsOrOpts as StartOptions);
  }
}
