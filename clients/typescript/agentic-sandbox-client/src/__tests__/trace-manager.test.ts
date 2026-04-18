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

import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Mock } from "vitest";

import { TracerManager, withSpan, NoOpSpan } from "../trace-manager.js";
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
  startActiveSpan: Mock;
  startSpan: Mock;
  spans: Array<{ name: string; span: FakeSpan }>;
} {
  const spans: Array<{ name: string; span: FakeSpan }> = [];
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
  const tracer = { startSpan, startActiveSpan } as unknown as Tracer;
  return { tracer, startSpan, startActiveSpan, spans };
}

// ---------- tests ----------

describe("withSpan", () => {
  // ===== real tracer path =====

  describe("with a fake tracer", () => {
    it("calls tracer.startActiveSpan with the composed span name", async () => {
      const { tracer, startActiveSpan } = makeFakeTracer();

      await withSpan(tracer, "my-service", "do-work", (span) => {
        span.setAttribute("key", "value");
      });

      expect(startActiveSpan).toHaveBeenCalledWith(
        "my-service.do-work",
        expect.any(Function),
      );
    });

    it("calls span.end() after the callback completes", async () => {
      const { tracer, spans } = makeFakeTracer();

      await withSpan(tracer, "svc", "op", () => "done");

      expect(spans[0].span.end).toHaveBeenCalled();
    });

    it("calls span.end() even when the callback throws", async () => {
      const { tracer, spans } = makeFakeTracer();

      await withSpan(tracer, "svc", "op", () => {
        throw new Error("intentional");
      }).catch(() => {});

      expect(spans[0].span.end).toHaveBeenCalled();
    });

    it("records exception on span when callback throws", async () => {
      const { tracer, spans } = makeFakeTracer();
      const err = new Error("boom");

      await withSpan(tracer, "svc", "op", () => {
        throw err;
      }).catch(() => {});

      expect(spans[0].span.recordException).toHaveBeenCalledWith(err);
    });

    it("sets error status on span when callback throws", async () => {
      const { tracer, spans } = makeFakeTracer();

      await withSpan(tracer, "svc", "op", () => {
        throw new Error("boom");
      }).catch(() => {});

      expect(spans[0].span.setStatus).toHaveBeenCalledWith(
        expect.objectContaining({ message: "boom" }),
      );
    });

    it("rethrows the original error after recording", async () => {
      const { tracer } = makeFakeTracer();

      await expect(
        withSpan(tracer, "svc", "op", () => {
          throw new Error("boom");
        }),
      ).rejects.toThrow("boom");
    });

    it("records non-Error thrown values using String()", async () => {
      const { tracer, spans } = makeFakeTracer();

      await withSpan(tracer, "svc", "op", () => {
        throw "oops";
      }).catch(() => {});

      expect(spans[0].span.setStatus).toHaveBeenCalledWith(
        expect.objectContaining({ message: "oops" }),
      );
    });

    it("returns the value from the callback", async () => {
      const { tracer } = makeFakeTracer();

      const result = await withSpan(tracer, "svc", "op", () => 42);

      expect(result).toBe(42);
    });
  });

  // ===== no-op tracer path (Go: TestTracingNoopWithoutProvider equivalent) =====

  describe("with null tracer (no-op path)", () => {
    it("calls the callback with a NoOpSpan that is not recording", async () => {
      let capturedSpan: Span | null = null;

      await withSpan(null, "svc", "op", (span) => {
        capturedSpan = span;
      });

      expect(capturedSpan).toBeInstanceOf(NoOpSpan);
      expect(capturedSpan!.isRecording()).toBe(false);
    });

    it("returns the callback value without throwing", async () => {
      const result = await withSpan(null, "svc", "op", () => "result");

      expect(result).toBe("result");
    });
  });
});

// ===== TracerManager in no-op mode (Go: TestTracingNoopWithoutProvider equivalent) =====
//
// otelApi is null at module-load time (initializeTracer() has not been called),
// so TracerManager always constructs with NoOpTracer in these tests.

describe("TracerManager (no-op mode — otelApi === null)", () => {
  it("constructs without throwing", () => {
    expect(() => new TracerManager("test-service")).not.toThrow();
  });

  it("startLifecycleSpan / endLifecycleSpan / withSpan complete without throwing", async () => {
    const mgr = new TracerManager("test-service");

    mgr.startLifecycleSpan();

    await withSpan(mgr.tracer, "test-service", "read", (span) => {
      span.setAttribute("sandbox.file.path", "test.txt");
    });

    mgr.endLifecycleSpan();
  });

  it("getTraceContextJson returns empty string when otelApi is null", () => {
    const mgr = new TracerManager("test-service");
    mgr.startLifecycleSpan();

    expect(mgr.getTraceContextJson()).toBe("");
  });

  it("endLifecycleSpan clears parentContext", () => {
    const mgr = new TracerManager("test-service");
    mgr.startLifecycleSpan();
    mgr.endLifecycleSpan();

    // After end, parentContext is null → getTraceContextJson returns ""
    expect(mgr.getTraceContextJson()).toBe("");
  });
});
