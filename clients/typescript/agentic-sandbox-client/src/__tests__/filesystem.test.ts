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
import { Filesystem } from "../files/filesystem.js";
import type { Span, Tracer } from "../trace-manager.js";

// ---------- helpers ----------

interface FakeSpan extends Span {
  end: Mock;
  setAttribute: Mock;
  recordException: Mock;
  setStatus: Mock;
}

function makeFakeTracer(): {
  tracer: Tracer;
  spans: Array<{ name: string; span: FakeSpan }>;
} {
  const spans: Array<{ name: string; span: FakeSpan }> = [];
  const startActiveSpan = vi.fn((name: string, fn: (span: Span) => unknown) => {
    const span: FakeSpan = {
      isRecording: () => true,
      setAttribute: vi.fn(),
      recordException: vi.fn(),
      setStatus: vi.fn(),
      end: vi.fn(),
    };
    spans.push({ name, span });
    return fn(span);
  });
  const startSpan = vi.fn((name: string) => {
    const span: FakeSpan = {
      isRecording: () => true,
      setAttribute: vi.fn(),
      recordException: vi.fn(),
      setStatus: vi.fn(),
      end: vi.fn(),
    };
    spans.push({ name, span });
    return span;
  });
  const tracer = { startSpan, startActiveSpan } as unknown as Tracer;
  return { tracer, spans };
}

function makeFilesystem(tracer: Tracer | null): Filesystem {
  const requestFn = vi
    .fn()
    .mockResolvedValue(new Response("ok", { status: 200 }));
  return new Filesystem(requestFn, () => tracer, "test-service");
}

// ---------- tests ----------

describe("Filesystem.write — sandbox.file.size trace attribute", () => {
  it("records byte length for ASCII string content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);

    await fs.write("file.txt", "hello");

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBe(5);
  });

  it("records byte length (not character count) for non-ASCII string content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);
    const content = "こんにちは"; // 5 chars, 15 bytes in UTF-8

    await fs.write("file.txt", content);

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBeGreaterThan(content.length);
    expect(sizeCall?.[1]).toBe(Buffer.byteLength(content));
  });

  it("records byte length for Buffer content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);
    const buf = Buffer.from([0x01, 0x02, 0x03]);

    await fs.write("file.bin", buf);

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBe(3);
  });
});
