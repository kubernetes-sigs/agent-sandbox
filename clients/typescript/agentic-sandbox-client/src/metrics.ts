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

import { noopLogger } from "./logger.js";
import type { Logger } from "./types.js";

interface HistogramOptions {
  description?: string;
  unit?: string;
  advice?: { explicitBucketBoundaries?: number[] };
}

interface Histogram {
  record(value: number, attributes?: Record<string, string>): void;
}

interface Meter {
  createHistogram(name: string, options?: HistogramOptions): Histogram;
}

interface MeterProvider {
  getMeter(name: string): Meter;
}

interface OtelMetricsApi {
  getMeterProvider(): MeterProvider;
}

class NoOpHistogram implements Histogram {
  record(_value: number, _attributes?: Record<string, string>): void {}
}

interface LatencyInstruments {
  discoveryLatency: Histogram;
  suspendLatency: Histogram;
  resumeLatency: Histogram;
}

const NOOP_INSTRUMENTS: LatencyInstruments = {
  discoveryLatency: new NoOpHistogram(),
  suspendLatency: new NoOpHistogram(),
  resumeLatency: new NoOpHistogram(),
};

let otelMetricsLoadPromise: Promise<OtelMetricsApi | null> | null = null;

function loadOtelMetrics(): Promise<OtelMetricsApi | null> {
  if (!otelMetricsLoadPromise) {
    otelMetricsLoadPromise = (async () => {
      try {
        const api = await import("@opentelemetry/api");
        return api.metrics as unknown as OtelMetricsApi;
      } catch {
        return null;
      }
    })();
  }
  return otelMetricsLoadPromise;
}

// Instrument names follow the OTel convention of keeping the unit out of the
// name and declaring it with `unit: "s"` instead. The OTel Prometheus
// exporter sanitizes dots to underscores and appends the unit, so these
// histograms export as sandbox_client_discovery_latency_seconds,
// sandbox_client_suspend_latency_seconds, and
// sandbox_client_resume_latency_seconds — the same names the Go SDK uses.
// The Python SDK still exports millisecond histograms named
// sandbox_client_*_latency_ms until #1902 merges. The Python SDK's snapshot
// restore metric has no counterpart here: this SDK does not support snapshots.
const DISCOVERY_LATENCY_INSTRUMENT = "sandbox.client.discovery.latency";
const SUSPEND_LATENCY_INSTRUMENT = "sandbox.client.suspend.latency";
const RESUME_LATENCY_INSTRUMENT = "sandbox.client.resume.latency";

// The Python SDK's _LATENCY_BUCKETS_MS expressed in seconds, so cross-SDK
// dashboards aggregate identically.
const LATENCY_BUCKETS_SECONDS = [
  0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 5, 10, 30, 60, 120, 240,
];

function latencyOptions(description: string): HistogramOptions {
  return {
    description,
    unit: "s",
    advice: { explicitBucketBoundaries: LATENCY_BUCKETS_SECONDS },
  };
}

/**
 * Holds the client's latency histograms. The metrics API has no proxy layer,
 * so instruments are resolved from the current global MeterProvider on each
 * access and cached per provider: a provider registered after this manager
 * was created still records. Without `@opentelemetry/api`, or without a
 * registered provider, every histogram is a no-op.
 */
export class MetricsManager {
  private readonly api: OtelMetricsApi | null;
  private readonly scope: string;
  private readonly logger: Logger;
  private cachedProvider: MeterProvider | null = null;
  private cachedInstruments: LatencyInstruments | null = null;

  private constructor(
    api: OtelMetricsApi | null,
    scope: string,
    logger: Logger,
  ) {
    this.api = api;
    this.scope = scope;
    this.logger = logger;
  }

  /**
   * Loads the global OpenTelemetry metrics API. Falls back to no-op
   * histograms when `@opentelemetry/api` is not installed; metrics must
   * never break sandbox operations.
   */
  static async create(
    serviceName: string,
    logger: Logger = noopLogger,
  ): Promise<MetricsManager> {
    const api = await loadOtelMetrics();
    if (!api) {
      logger.debug(
        "OpenTelemetry not installed; sandbox client metrics are disabled.",
      );
    }
    return new MetricsManager(api, serviceName.replace(/-/g, "_"), logger);
  }

  /** A manager whose histograms discard every sample. */
  static noop(): MetricsManager {
    return new MetricsManager(null, "", noopLogger);
  }

  private instruments(): LatencyInstruments {
    if (!this.api) return NOOP_INSTRUMENTS;
    const provider = this.api.getMeterProvider();
    if (this.cachedInstruments && this.cachedProvider === provider) {
      return this.cachedInstruments;
    }
    let instruments: LatencyInstruments;
    try {
      const meter = provider.getMeter(this.scope);
      instruments = {
        discoveryLatency: meter.createHistogram(
          DISCOVERY_LATENCY_INSTRUMENT,
          latencyOptions("Latency of establishing connection to the sandbox"),
        ),
        suspendLatency: meter.createHistogram(
          SUSPEND_LATENCY_INSTRUMENT,
          latencyOptions("Latency of suspending the sandbox"),
        ),
        resumeLatency: meter.createHistogram(
          RESUME_LATENCY_INSTRUMENT,
          latencyOptions("Latency of resuming the sandbox"),
        ),
      };
    } catch (err) {
      this.logger.error(
        `Failed to create OpenTelemetry histograms; sandbox client metrics are disabled: ${err}`,
      );
      instruments = NOOP_INSTRUMENTS;
    }
    this.cachedProvider = provider;
    this.cachedInstruments = instruments;
    return instruments;
  }

  get discoveryLatency(): Histogram {
    return this.instruments().discoveryLatency;
  }

  get suspendLatency(): Histogram {
    return this.instruments().suspendLatency;
  }

  get resumeLatency(): Histogram {
    return this.instruments().resumeLatency;
  }
}

/**
 * Records the seconds elapsed since `start` (a `performance.now()`
 * timestamp) on `histogram` with a `status` attribute derived from
 * `outcome`, matching the Python SDK's record_latency decorator. A throwing
 * `record()` is suppressed: callers invoke this from finally blocks, where an
 * exception would replace the operation's own result or error.
 */
export function recordLatency(
  histogram: Histogram,
  start: number,
  outcome: "success" | "failure",
  extraAttributes?: Record<string, string>,
): void {
  try {
    histogram.record((performance.now() - start) / 1000, {
      status: outcome,
      ...extraAttributes,
    });
  } catch {
    // The global MeterProvider is application-supplied; metrics must never
    // alter the outcome of a sandbox operation.
  }
}

// Histogram stays exported: it names the parameter type of recordLatency.
export type { Histogram };
