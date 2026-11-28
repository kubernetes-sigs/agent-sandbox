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

import type { Mock } from "vitest";
import { afterEach, describe, expect, it, vi } from "vitest";

// metrics.ts caches the @opentelemetry/api import at module level, so every
// test resets the module registry and re-imports it against a fresh mock.
afterEach(() => {
  vi.resetModules();
  vi.doUnmock("@opentelemetry/api");
});

interface CreatedHistogram {
  name: string;
  options?: {
    description?: string;
    unit?: string;
    advice?: { explicitBucketBoundaries?: number[] };
  };
  record: Mock;
}

async function importWithFakeOtel(): Promise<{
  metrics: typeof import("../metrics.js");
  created: CreatedHistogram[];
  getMeter: Mock;
}> {
  const created: CreatedHistogram[] = [];
  const getMeter = vi.fn(() => ({
    createHistogram: (name: string, options?: CreatedHistogram["options"]) => {
      const record = vi.fn();
      created.push({ name, options, record });
      return { record };
    },
  }));
  // A stable provider object: MetricsManager caches instruments per provider
  // identity, so a fresh object per call would defeat the cache.
  const provider = { getMeter };
  vi.doMock("@opentelemetry/api", () => ({
    metrics: { getMeterProvider: () => provider },
  }));
  return { metrics: await import("../metrics.js"), created, getMeter };
}

describe("MetricsManager", () => {
  it("creates the three latency histograms with OTel conventions", async () => {
    const { metrics, created, getMeter } = await importWithFakeOtel();

    const m = await metrics.MetricsManager.create("sandbox-client");
    // Instruments are resolved from the current provider on first access.
    expect(m.discoveryLatency).toBeDefined();
    expect(m.suspendLatency).toBeDefined();
    expect(m.resumeLatency).toBeDefined();

    // The scope mirrors TracerManager: dashes become underscores.
    expect(getMeter).toHaveBeenCalledWith("sandbox_client");
    expect(created.map((h) => h.name)).toEqual([
      "sandbox.client.discovery.latency",
      "sandbox.client.suspend.latency",
      "sandbox.client.resume.latency",
    ]);
    // Buckets must match the Go SDK's boundaries: the Python SDK's
    // _LATENCY_BUCKETS_MS expressed in seconds.
    const wantBuckets = [
      0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 5, 10, 30, 60, 120, 240,
    ];
    for (const h of created) {
      expect(h.options?.unit).toBe("s");
      expect(h.options?.advice?.explicitBucketBoundaries).toEqual(wantBuckets);
    }
    expect(created.map((h) => h.options?.description)).toEqual([
      "Latency of establishing connection to the sandbox",
      "Latency of suspending the sandbox",
      "Latency of resuming the sandbox",
    ]);
  });

  it("falls back to no-op histograms when @opentelemetry/api is not installed", async () => {
    vi.doMock("@opentelemetry/api", () => {
      const err = new Error("Cannot find package '@opentelemetry/api'");
      (err as unknown as { code: string }).code = "ERR_MODULE_NOT_FOUND";
      throw err;
    });
    const { MetricsManager } = await import("../metrics.js");

    const m = await MetricsManager.create("sandbox-client");

    expect(() =>
      m.discoveryLatency.record(1.5, {
        mode: "sandboxd_pod_tunnel",
        status: "success",
      }),
    ).not.toThrow();
  });

  it("noop() discards samples", async () => {
    const { metrics } = await importWithFakeOtel();

    const m = metrics.MetricsManager.noop();

    expect(() => m.suspendLatency.record(1)).not.toThrow();
  });

  it("records through a MeterProvider registered after create()", async () => {
    // Uses the real @opentelemetry/api: the manager is created while no
    // global provider is registered, then the provider appears and later
    // records must reach it.
    const { MetricsManager } = await import("../metrics.js");
    const m = await MetricsManager.create("sandbox-client");

    const record = vi.fn();
    const provider = {
      getMeter: () => ({ createHistogram: () => ({ record }) }),
    };
    const api = await import("@opentelemetry/api");
    try {
      api.metrics.setGlobalMeterProvider(provider as never);
      m.suspendLatency.record(0.5, { status: "success" });
      expect(record).toHaveBeenCalledWith(0.5, { status: "success" });
    } finally {
      api.metrics.disable();
    }
  });
});

describe("recordLatency", () => {
  it("records elapsed seconds with a status attribute", async () => {
    const { recordLatency } = await import("../metrics.js");
    const record = vi.fn();

    const start = performance.now() - 1500;
    recordLatency({ record }, start, "failure");

    expect(record).toHaveBeenCalledTimes(1);
    const [value, attrs] = record.mock.calls[0];
    expect(value).toBeGreaterThanOrEqual(1.5);
    expect(value).toBeLessThan(10);
    expect(attrs).toEqual({ status: "failure" });
  });

  it("merges extra attributes alongside the status", async () => {
    const { recordLatency } = await import("../metrics.js");
    const record = vi.fn();

    recordLatency({ record }, performance.now(), "success", {
      mode: "sandboxd_pod_tunnel",
    });

    expect(record.mock.calls[0][1]).toEqual({
      status: "success",
      mode: "sandboxd_pod_tunnel",
    });
  });

  it("suppresses a throwing record() so callers' finally blocks stay safe", async () => {
    const { recordLatency } = await import("../metrics.js");
    const record = vi.fn(() => {
      throw new Error("provider exploded");
    });

    expect(() =>
      recordLatency({ record }, performance.now(), "success"),
    ).not.toThrow();
    expect(record).toHaveBeenCalledTimes(1);
  });
});
