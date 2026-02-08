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

interface Span {
  isRecording(): boolean;
  setAttribute(key: string, value: string | number | boolean): void;
  end(): void;
}

interface Tracer {
  startSpan(name: string): Span;
  startActiveSpan<T>(name: string, fn: (span: Span) => T): T;
}

interface OtelApi {
  trace: {
    getTracer(name: string): Tracer;
    setSpanInContext(span: Span, ctx: unknown): unknown;
    getSpan(ctx: unknown): Span | undefined;
  };
  context: {
    active(): unknown;
    with<T>(ctx: unknown, fn: () => T): T;
    bind<T extends (...args: unknown[]) => unknown>(ctx: unknown, fn: T): T;
  };
  propagation: {
    inject(ctx: unknown, carrier: Record<string, string>): void;
  };
}

class NoOpSpan implements Span {
  isRecording(): boolean {
    return false;
  }
  setAttribute(_key: string, _value: string | number | boolean): void {}
  end(): void {}
}

class NoOpTracer implements Tracer {
  startSpan(_name: string): Span {
    return new NoOpSpan();
  }
  startActiveSpan<T>(_name: string, fn: (span: Span) => T): T {
    return fn(new NoOpSpan());
  }
}

let otelApi: OtelApi | null = null;
let otelLoaded = false;

async function loadOtel(): Promise<OtelApi | null> {
  if (otelLoaded) return otelApi;
  otelLoaded = true;
  try {
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment
    const api: Record<string, unknown> = await (
      Function('return import("@opentelemetry/api")')() as Promise<
        Record<string, unknown>
      >
    );
    otelApi = {
      trace: api.trace as OtelApi["trace"],
      context: api.context as OtelApi["context"],
      propagation: api.propagation as OtelApi["propagation"],
    };
    return otelApi;
  } catch {
    return null;
  }
}

let tracerProviderInitialized = false;
let tracerProviderServiceName: string | null = null;

export async function initializeTracer(serviceName: string): Promise<void> {
  const api = await loadOtel();
  if (!api) {
    console.error(
      "OpenTelemetry not installed; skipping tracer initialization.",
    );
    return;
  }

  if (tracerProviderInitialized) {
    if (
      tracerProviderServiceName &&
      tracerProviderServiceName !== serviceName
    ) {
      console.warn(
        `Global TracerProvider already initialized with service name '${tracerProviderServiceName}'. ` +
          `Ignoring request to initialize with '${serviceName}'.`,
      );
    }
    return;
  }

  try {
    const dynamicImport = Function(
      "specifier",
      "return import(specifier)",
    ) as (specifier: string) => Promise<Record<string, unknown>>;

    const sdkTraceNode = await dynamicImport(
      "@opentelemetry/sdk-trace-node",
    );
    const resources = await dynamicImport("@opentelemetry/resources");
    const sdkTraceBase = await dynamicImport(
      "@opentelemetry/sdk-trace-base",
    );
    const exporterOtlpGrpc = await dynamicImport(
      "@opentelemetry/exporter-trace-otlp-grpc",
    );

    const Resource = resources.Resource as new (
      attrs: Record<string, string>,
    ) => unknown;
    const NodeTracerProvider = sdkTraceNode.NodeTracerProvider as new (
      opts: { resource: unknown },
    ) => {
      addSpanProcessor(processor: unknown): void;
      register(): void;
    };
    const BatchSpanProcessor = sdkTraceBase.BatchSpanProcessor as new (
      exporter: unknown,
    ) => unknown;
    const OTLPTraceExporter =
      exporterOtlpGrpc.OTLPTraceExporter as new () => unknown;

    const resource = new Resource({ "service.name": serviceName });
    const provider = new NodeTracerProvider({ resource });
    provider.addSpanProcessor(
      new BatchSpanProcessor(new OTLPTraceExporter()),
    );
    provider.register();

    tracerProviderInitialized = true;
    tracerProviderServiceName = serviceName;
    console.info(
      `Global OpenTelemetry TracerProvider configured for service '${serviceName}'.`,
    );
  } catch {
    console.warn(
      "OpenTelemetry SDK packages not installed; tracer provider not configured. " +
        "Tracing spans will use the default no-op provider.",
    );
  }
}

export async function withSpan<T>(
  tracer: Tracer | null,
  serviceName: string,
  spanSuffix: string,
  fn: (span: Span) => T | Promise<T>,
): Promise<T> {
  if (!tracer) {
    return fn(new NoOpSpan());
  }
  const spanName = `${serviceName}.${spanSuffix}`;
  return tracer.startActiveSpan(spanName, async (span) => {
    try {
      return await fn(span);
    } finally {
      span.end();
    }
  });
}

export function getCurrentSpan(): Span {
  if (!otelApi) return new NoOpSpan();
  const span = otelApi.trace.getSpan(otelApi.context.active());
  return span ?? new NoOpSpan();
}

export class TracerManager {
  public tracer: Tracer;
  private lifecycleSpanName: string;
  private parentSpan: Span | null = null;
  private parentContext: unknown = null;

  constructor(serviceName: string) {
    const scopeName = serviceName.replace(/-/g, "_");
    if (otelApi) {
      this.tracer = otelApi.trace.getTracer(scopeName);
    } else {
      this.tracer = new NoOpTracer();
    }
    this.lifecycleSpanName = `${serviceName}.lifecycle`;
  }

  startLifecycleSpan(): void {
    this.parentSpan = this.tracer.startSpan(this.lifecycleSpanName);
    if (otelApi && this.parentSpan) {
      this.parentContext = otelApi.trace.setSpanInContext(
        this.parentSpan,
        otelApi.context.active(),
      );
    }
  }

  endLifecycleSpan(): void {
    if (this.parentSpan) {
      this.parentSpan.end();
      this.parentSpan = null;
    }
    this.parentContext = null;
  }

  getTraceContextJson(): string {
    if (!otelApi || !this.parentContext) return "";
    const carrier: Record<string, string> = {};
    otelApi.propagation.inject(this.parentContext, carrier);
    return Object.keys(carrier).length > 0 ? JSON.stringify(carrier) : "";
  }
}

export { NoOpSpan, NoOpTracer };
export type { Span, Tracer };
