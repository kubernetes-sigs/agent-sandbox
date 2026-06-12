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

import { MAX_EXECUTION_RESPONSE_SIZE } from "../constants.js";
import { SandboxRequestError } from "../exceptions.js";
import { parseExecutionResult, readBoundedText } from "../response-utils.js";
import type { Tracer } from "../trace-manager.js";
import { withSpan } from "../trace-manager.js";
import type { CallOptions, ExecutionResult, RequestFn } from "../types.js";

function normalizeCallOptions(
  arg: number | CallOptions | undefined,
  defaultTimeoutSec: number,
): { timeout: number; signal?: AbortSignal } {
  if (typeof arg === "number") {
    return { timeout: arg };
  }
  if (arg === undefined) {
    return { timeout: defaultTimeoutSec };
  }
  return {
    timeout: arg.timeout ?? defaultTimeoutSec,
    signal: arg.signal,
  };
}

export class CommandExecutor {
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

  async run(
    command: string,
    options?: number | CallOptions,
  ): Promise<ExecutionResult> {
    const { timeout, signal } = normalizeCallOptions(options, 60);
    return withSpan(
      this.getTracer(),
      this.traceServiceName,
      "run",
      async (span) => {
        if (span.isRecording()) {
          span.setAttribute("sandbox.command", command);
        }

        const response = await this.requestFn("POST", "execute", {
          body: JSON.stringify({ command }),
          headers: { "Content-Type": "application/json" },
          timeout,
          signal,
          maxRetries: 1, // command execution is non-idempotent; never retry
        });

        const rawText = await readBoundedText(
          response,
          MAX_EXECUTION_RESPONSE_SIZE,
          "execute",
        );
        let data: unknown;
        try {
          data = JSON.parse(rawText);
        } catch (err) {
          throw new SandboxRequestError(
            `Failed to decode JSON response from sandbox: ${rawText}`,
            { cause: err },
          );
        }
        const result = parseExecutionResult(data);

        if (span.isRecording()) {
          span.setAttribute("sandbox.exit_code", result.exitCode);
        }

        return result;
      },
      this.getParentContext(),
    );
  }
}
