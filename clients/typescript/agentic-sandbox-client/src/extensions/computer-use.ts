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
import { SandboxNotReadyError, SandboxRequestError } from "../exceptions.js";
import { parseExecutionResult, readBoundedText } from "../response-utils.js";
import type { SandboxInit } from "../sandbox.js";
import { Sandbox } from "../sandbox.js";
import { SandboxClient } from "../sandbox-client.js";
import { withSpan } from "../trace-manager.js";
import type { ExecutionResult } from "../types.js";

/**
 * Sandbox handle with computer-use agent support.
 * Use ComputerUseSandboxClient to create instances.
 */
export class ComputerUseSandbox extends Sandbox {
  async agent(query: string, timeout: number = 60): Promise<ExecutionResult> {
    return withSpan(
      this.tracer,
      this.traceServiceName,
      "agent",
      async (span) => {
        if (!this.isActive) {
          throw new SandboxNotReadyError(
            "Sandbox is not ready. Cannot execute agent queries.",
          );
        }

        if (span.isRecording()) {
          span.setAttribute("sandbox.agent.query", query);
        }

        const response = await this.request("POST", "agent", {
          body: JSON.stringify({ query }),
          headers: { "Content-Type": "application/json" },
          timeout,
          maxRetries: 1, // agent invocation is non-idempotent; never retry
        });

        const rawText = await readBoundedText(
          response,
          MAX_EXECUTION_RESPONSE_SIZE,
          "agent",
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
      this.tracingManager?.parentContext,
    );
  }
}

/**
 * Registry client that creates ComputerUseSandbox handles.
 */
export class ComputerUseSandboxClient extends SandboxClient<ComputerUseSandbox> {
  protected override readonly sandboxClass =
    ComputerUseSandbox as unknown as new (
      init: SandboxInit,
    ) => ComputerUseSandbox;
}
