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
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Span, Tracer } from "../trace-manager.js";
import { NoOpSpan, TracerManager, withSpan } from "../trace-manager.js";

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
      // biome-ignore lint/style/noNonNullAssertion: capturedSpan is set by the withSpan callback above
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

// ===== initializeTracer — parallel call safety =====
//
// Verifies that concurrent loadOtel() calls share a single in-flight Promise,
// so the second caller never receives null while the first import is pending.

describe("initializeTracer — parallel call safety", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("concurrent calls share the in-flight loadOtel promise and do not race on otelApi", async () => {
    const consoleErrorSpy = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});

    vi.doMock("@opentelemetry/api", () => ({
      SpanStatusCode: { ERROR: 2 },
      trace: {
        getTracer: vi.fn(),
        setSpanInContext: vi.fn((_s: unknown, ctx: unknown) => ctx),
        getSpan: vi.fn(),
      },
      context: {
        active: vi.fn(() => ({})),
        with: vi.fn((_: unknown, fn: () => unknown) => fn()),
        bind: vi.fn(),
      },
      propagation: { inject: vi.fn() },
    }));

    // Fresh import after doMock so module-level state (otelLoadPromise) is clean.
    const { initializeTracer } = await import("../trace-manager.js");

    // Fire in parallel. With the race bug, the second call sees otelLoaded=true
    // with otelApi still null and emits console.error("OpenTelemetry not installed").
    await Promise.all([
      initializeTracer("my-service"),
      initializeTracer("my-service"),
    ]);

    expect(consoleErrorSpy).not.toHaveBeenCalledWith(
      expect.stringContaining("OpenTelemetry not installed"),
    );
  });

  it("provider.register() is called exactly once when initializeTracer runs in parallel with full SDK mocked", async () => {
    const registerMock = vi.fn();

    vi.doMock("@opentelemetry/api", () => ({
      SpanStatusCode: { ERROR: 2 },
      trace: {
        getTracer: vi.fn(),
        setSpanInContext: vi.fn((_s: unknown, ctx: unknown) => ctx),
        getSpan: vi.fn(),
      },
      context: {
        active: vi.fn(() => ({})),
        with: vi.fn((_: unknown, fn: () => unknown) => fn()),
        bind: vi.fn(),
      },
      propagation: { inject: vi.fn() },
    }));
    vi.doMock("@opentelemetry/sdk-trace-node", () => ({
      NodeTracerProvider: class {
        addSpanProcessor() {}
        register = registerMock;
        async shutdown() {}
      },
    }));
    vi.doMock("@opentelemetry/resources", () => ({ Resource: class {} }));
    vi.doMock("@opentelemetry/sdk-trace-base", () => ({
      BatchSpanProcessor: class {},
    }));
    vi.doMock("@opentelemetry/exporter-trace-otlp-grpc", () => ({
      OTLPTraceExporter: class {},
    }));

    // Fresh import after doMock so module-level state (tracerInitPromise) is clean.
    const { initializeTracer } = await import("../trace-manager.js");

    await Promise.all([
      initializeTracer("my-service"),
      initializeTracer("my-service"),
    ]);

    // Without the tracerInitPromise cache, both concurrent calls would each
    // create a NodeTracerProvider and call register(), resulting in 2 invocations.
    expect(registerMock).toHaveBeenCalledTimes(1);
  });
});

// ===== initializeTracer — beforeExit shutdown error handling =====
//
// Verifies that a provider.shutdown() rejection is caught and logged rather than
// propagating as an unhandled rejection from the beforeExit handler.

describe("initializeTracer — beforeExit shutdown error handling", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("shutdown failure is caught and logged, not thrown as unhandled rejection", async () => {
    const consoleErrorSpy = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});

    // Use an object so TypeScript does not narrow the property type to null via
    // control-flow analysis (local let variables assigned inside callbacks are
    // narrowed to their initialiser type at subsequent reads).
    const captured: { beforeExit: (() => void) | null } = { beforeExit: null };
    vi.spyOn(process, "once").mockImplementation(
      (event: string | symbol, handler: (...args: unknown[]) => void) => {
        if (event === "beforeExit") {
          captured.beforeExit = handler as () => void;
        }
        return process;
      },
    );

    const shutdownMock = vi
      .fn()
      .mockRejectedValue(new Error("shutdown failed"));

    vi.doMock("@opentelemetry/api", () => ({
      SpanStatusCode: { ERROR: 2 },
      trace: {
        getTracer: vi.fn(),
        setSpanInContext: vi.fn((_s: unknown, ctx: unknown) => ctx),
        getSpan: vi.fn(),
      },
      context: {
        active: vi.fn(() => ({})),
        with: vi.fn((_: unknown, fn: () => unknown) => fn()),
        bind: vi.fn(),
      },
      propagation: { inject: vi.fn() },
    }));
    vi.doMock("@opentelemetry/sdk-trace-node", () => ({
      NodeTracerProvider: class {
        addSpanProcessor() {}
        register() {}
        shutdown = shutdownMock;
      },
    }));
    vi.doMock("@opentelemetry/resources", () => ({
      Resource: class {},
    }));
    vi.doMock("@opentelemetry/sdk-trace-base", () => ({
      BatchSpanProcessor: class {},
    }));
    vi.doMock("@opentelemetry/exporter-trace-otlp-grpc", () => ({
      OTLPTraceExporter: class {},
    }));

    const { initializeTracer } = await import("../trace-manager.js");
    await initializeTracer("my-service");

    expect(captured.beforeExit).not.toBeNull();

    // Trigger the handler manually and flush microtasks.
    captured.beforeExit?.();
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    expect(consoleErrorSpy).toHaveBeenCalledWith(
      expect.stringContaining("shutdown failed"),
      expect.any(Error),
    );
  });
});
