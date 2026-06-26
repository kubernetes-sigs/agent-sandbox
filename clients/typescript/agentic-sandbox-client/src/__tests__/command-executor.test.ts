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

import type { Mock } from "vitest";
import { describe, expect, it, vi } from "vitest";
import {
  CommandExecutor,
  extractExecutable,
} from "../commands/command-executor.js";
import { MAX_ERROR_BODY_BYTES } from "../constants.js";
import { SandboxRequestError } from "../exceptions.js";
import type { Span, Tracer } from "../trace-manager.js";

describe("extractExecutable", () => {
  it.each([
    ["ls -la", "ls"],
    ["/usr/bin/python3 script.py", "python3"],
    ["FOO=bar BAZ=qux ./run.sh arg", "run.sh"],
    ["", ""],
    ["FOO=bar", ""],
    [" ls -la", "ls"],
    ["/opt/graalvm-ce=22/bin/java -jar app.jar", "java"],
  ])("extractExecutable(%j) === %j", (input, expected) => {
    expect(extractExecutable(input)).toBe(expected);
  });
});

describe("CommandExecutor.run() — JSON decode error truncation", () => {
  it("truncates body >MAX_ERROR_BODY_BYTES in the error message", async () => {
    const longBody = "x".repeat(MAX_ERROR_BODY_BYTES + 1);
    const mockRequestFn = vi
      .fn()
      .mockResolvedValue(new Response(longBody, { status: 200 }));
    const executor = new CommandExecutor(mockRequestFn, () => null, "svc");

    const err = await executor.run("ls").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain("…");
    expect(err.message).not.toContain(longBody);
  });

  it("does not truncate body <=MAX_ERROR_BODY_BYTES in the error message", async () => {
    const shortBody = "x".repeat(MAX_ERROR_BODY_BYTES);
    const mockRequestFn = vi
      .fn()
      .mockResolvedValue(new Response(shortBody, { status: 200 }));
    const executor = new CommandExecutor(mockRequestFn, () => null, "svc");

    const err = await executor.run("ls").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain(shortBody);
    expect(err.message).not.toContain("…");
  });
});

describe("CommandExecutor.run() span attributes", () => {
  it("sets sandbox.command.executable and does not set sandbox.command", async () => {
    const spans: Array<{ setAttribute: Mock }> = [];
    const fakeTracer = {
      startActiveSpan: vi.fn((_name: string, fn: (span: Span) => unknown) => {
        const span = {
          isRecording: () => true,
          setAttribute: vi.fn(),
          recordException: vi.fn(),
          setStatus: vi.fn(),
          end: vi.fn(),
        };
        spans.push(span);
        return fn(span);
      }),
      startSpan: vi.fn(),
    } as unknown as Tracer;

    const mockRequestFn = vi
      .fn()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ stdout: "ok", stderr: "", exit_code: 0 }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

    const executor = new CommandExecutor(
      mockRequestFn,
      () => fakeTracer,
      "test-service",
    );

    await executor.run("FOO=bar /usr/bin/python3 script.py");

    expect(spans.length).toBeGreaterThan(0);
    for (const span of spans) {
      const calls = span.setAttribute.mock.calls.map((c: unknown[]) => c[0]);
      expect(calls).toContain("sandbox.command.executable");
      expect(calls).not.toContain("sandbox.command");
    }

    const allSetAttributeCalls = spans.flatMap(
      (s) => s.setAttribute.mock.calls as [string, unknown][],
    );
    const executableCall = allSetAttributeCalls.find(
      ([key]) => key === "sandbox.command.executable",
    );
    expect(executableCall?.[1]).toBe("python3");
  });

  it("does not set sandbox.command.executable when command has no executable (all env vars)", async () => {
    const spans: Array<{ setAttribute: Mock }> = [];
    const fakeTracer = {
      startActiveSpan: vi.fn((_name: string, fn: (span: Span) => unknown) => {
        const span = {
          isRecording: () => true,
          setAttribute: vi.fn(),
          recordException: vi.fn(),
          setStatus: vi.fn(),
          end: vi.fn(),
        };
        spans.push(span);
        return fn(span);
      }),
      startSpan: vi.fn(),
    } as unknown as Tracer;

    const mockRequestFn = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ stdout: "", stderr: "", exit_code: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const executor = new CommandExecutor(
      mockRequestFn,
      () => fakeTracer,
      "test-service",
    );

    await executor.run("FOO=bar BAZ=qux");

    expect(spans.length).toBeGreaterThan(0);
    for (const span of spans) {
      const keys = span.setAttribute.mock.calls.map((c: unknown[]) => c[0]);
      expect(keys).not.toContain("sandbox.command.executable");
    }
  });
});
