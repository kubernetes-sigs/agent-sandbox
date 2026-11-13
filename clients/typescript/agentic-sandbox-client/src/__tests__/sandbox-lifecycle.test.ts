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

import * as http from "node:http";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { mockGet, mockPatch, mockCreateStrategy } = vi.hoisted(() => ({
  mockGet: vi.fn(),
  mockPatch: vi.fn(),
  mockCreateStrategy: vi.fn(),
}));

vi.mock("@kubernetes/client-node", () => ({ CustomObjectsApi: vi.fn() }));
vi.mock("../connection.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../connection.js")>()),
  createConnectionStrategy: mockCreateStrategy,
}));

import {
  SandboxClosedError,
  SandboxError,
  SandboxNotFoundError,
  SandboxTimeoutError,
} from "../exceptions.js";
import type { SandboxInit } from "../sandbox.js";
import { normalizeSandboxdOptions, Sandbox } from "../sandbox.js";

interface FakeSandbox {
  mode?: string;
  generation?: number;
  uid?: string;
  podIPs?: string[];
  deleting?: boolean;
  conditions?: Array<{
    type: string;
    status: string;
    observedGeneration?: number;
  }>;
}

function sandboxObj(o: FakeSandbox = {}) {
  return {
    metadata: {
      uid: o.uid ?? "uid-1",
      generation: o.generation ?? 1,
      ...(o.deleting ? { deletionTimestamp: "2026-01-01T00:00:00Z" } : {}),
    },
    spec: o.mode ? { operatingMode: o.mode } : {},
    status: { podIPs: o.podIPs ?? [], conditions: o.conditions ?? [] },
  };
}

function cond(type: string, observedGeneration: number, status = "True") {
  return { type, status, observedGeneration };
}

function createInit(overrides: Partial<SandboxInit> = {}): SandboxInit {
  return {
    claimName: "test-claim",
    sandboxName: "test-sandbox",
    podName: "test-pod",
    podIP: "10.0.0.1",
    namespace: "default",
    customObjectsApi: {
      getNamespacedCustomObject: mockGet,
      patchNamespacedCustomObject: mockPatch,
      deleteNamespacedCustomObject: vi.fn().mockResolvedValue({}),
    } as unknown as import("@kubernetes/client-node").CustomObjectsApi,
    kubeConfig: {} as import("@kubernetes/client-node").KubeConfig,
    sandboxdOptions: normalizeSandboxdOptions(),
    tracingManager: null,
    traceServiceName: "test-service",
    ...overrides,
  };
}

const patchBody = (mode: string) => [
  { op: "test", path: "/metadata/uid", value: "uid-1" },
  { op: "add", path: "/spec/operatingMode", value: mode },
];

describe("Sandbox suspend/resume", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockCreateStrategy.mockImplementation((connectivity: string) => ({
      connectivity,
      open: vi.fn(),
    }));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe("isSuspended()", () => {
    it.each([
      ["Suspended", true],
      ["Running", false],
      [undefined, false],
    ])("reports %s as %s", async (mode, want) => {
      mockGet.mockResolvedValueOnce(sandboxObj({ mode }));
      await expect(new Sandbox(createInit()).isSuspended()).resolves.toBe(want);
    });

    it("rejects with SandboxNotFoundError when the Sandbox is gone", async () => {
      mockGet.mockRejectedValueOnce(
        Object.assign(new Error("x"), { code: 404 }),
      );
      await expect(new Sandbox(createInit()).isSuspended()).rejects.toThrow(
        SandboxNotFoundError,
      );
    });

    it("rejects with SandboxNotFoundError when the Sandbox is terminating", async () => {
      mockGet.mockResolvedValueOnce(sandboxObj({ deleting: true }));
      await expect(new Sandbox(createInit()).isSuspended()).rejects.toThrow(
        SandboxNotFoundError,
      );
    });
  });

  describe("suspend()", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    it("patches operatingMode and ignores a Suspended condition from an older generation", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Suspended",
            generation: 3,
            conditions: [cond("Suspended", 1)],
          }),
        )
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Suspended",
            generation: 3,
            conditions: [cond("Suspended", 3)],
          }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Suspended", generation: 3 }),
      );

      const done = new Sandbox(createInit()).suspend();
      await vi.advanceTimersByTimeAsync(500);
      await done;

      expect(mockPatch).toHaveBeenCalledExactlyOnceWith(
        expect.objectContaining({
          name: "test-sandbox",
          namespace: "default",
          body: patchBody("Suspended"),
        }),
      );
      expect(mockGet).toHaveBeenCalledTimes(3);
    });

    it("does not patch a Sandbox that is already suspended", async () => {
      mockGet
        .mockResolvedValueOnce(
          sandboxObj({ mode: "Suspended", conditions: [cond("Suspended", 1)] }),
        )
        .mockResolvedValueOnce(
          sandboxObj({ mode: "Suspended", conditions: [cond("Suspended", 1)] }),
        );

      await new Sandbox(createInit()).suspend();

      expect(mockPatch).not.toHaveBeenCalled();
    });

    it("rejects with SandboxTimeoutError when the controller never reports Suspended", async () => {
      mockGet.mockResolvedValue(sandboxObj({ mode: "Suspended" }));
      const result = expect(
        new Sandbox(createInit()).suspend({ timeoutMs: 1000 }),
      ).rejects.toThrow(SandboxTimeoutError);

      await vi.advanceTimersByTimeAsync(1500);
      await result;
    });

    it("rejects when the Sandbox is replaced while waiting", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended" }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended", uid: "other" }));

      await expect(new Sandbox(createInit()).suspend()).rejects.toThrow(
        /was replaced/,
      );
    });

    it("keeps the cause when the patch fails", async () => {
      const cause = Object.assign(new Error("forbidden"), { code: 403 });
      mockGet.mockResolvedValueOnce(sandboxObj({ mode: "Running" }));
      mockPatch.mockRejectedValueOnce(cause);

      const err = await new Sandbox(createInit()).suspend().catch((e) => e);

      expect(err).toBeInstanceOf(SandboxError);
      expect(err.cause).toBe(cause);
    });

    it("rejects an invalid timeoutMs before touching Kubernetes", async () => {
      await expect(
        new Sandbox(createInit()).suspend({ timeoutMs: 0 }),
      ).rejects.toThrow(SandboxError);
      expect(mockGet).not.toHaveBeenCalled();
    });

    it("rejects on a closed handle", async () => {
      const sandbox = new Sandbox(createInit());
      await sandbox.closeLocal();

      await expect(sandbox.suspend()).rejects.toThrow(SandboxClosedError);
      expect(mockGet).not.toHaveBeenCalled();
    });

    it("stops waiting when the caller aborts", async () => {
      mockGet.mockResolvedValue(sandboxObj({ mode: "Suspended" }));
      const controller = new AbortController();
      const reason = new Error("stop");
      const result = expect(
        new Sandbox(createInit()).suspend({ signal: controller.signal }),
      ).rejects.toBe(reason);

      await vi.advanceTimersByTimeAsync(100);
      controller.abort(reason);
      await result;
    });

    it("does not patch when the caller already aborted", async () => {
      mockGet.mockResolvedValue(sandboxObj({ mode: "Running" }));
      const reason = new Error("stop");

      await expect(
        new Sandbox(createInit()).suspend({
          signal: AbortSignal.abort(reason),
        }),
      ).rejects.toBe(reason);

      expect(mockPatch).not.toHaveBeenCalled();
    });

    it("does not patch when the caller aborts during the first read", async () => {
      const controller = new AbortController();
      const reason = new Error("stop");
      mockGet.mockImplementationOnce(async () => {
        controller.abort(reason);
        return sandboxObj({ mode: "Running" });
      });

      await expect(
        new Sandbox(createInit()).suspend({ signal: controller.signal }),
      ).rejects.toBe(reason);

      expect(mockPatch).not.toHaveBeenCalled();
    });

    it("expires a queued call without patching", async () => {
      // The first call polls for 30s; the queued call has a 1s budget.
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValue(sandboxObj({ mode: "Suspended" }));
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Suspended", generation: 2 }),
      );
      const sandbox = new Sandbox(createInit());
      const first = sandbox.suspend({ timeoutMs: 30_000 });
      const firstResult = expect(first).rejects.toThrow(SandboxTimeoutError);
      const queued = expect(
        sandbox.resume({ timeoutMs: 1000 }),
      ).rejects.toThrow(SandboxTimeoutError);

      await vi.advanceTimersByTimeAsync(1500);
      await queued;
      await vi.advanceTimersByTimeAsync(40_000);
      await firstResult;

      expect(mockPatch).toHaveBeenCalledTimes(1);
    });
  });

  describe("resume()", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    it("patches operatingMode, waits for Ready at the new generation, and adopts the new pod IP", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended", generation: 2 }))
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Running",
            generation: 3,
            conditions: [cond("Ready", 2)],
            podIPs: ["10.0.0.7"],
          }),
        )
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Running",
            generation: 3,
            conditions: [cond("Ready", 3)],
            podIPs: ["10.0.0.9"],
          }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Running", generation: 3 }),
      );
      const sandbox = new Sandbox(createInit());

      const done = sandbox.resume();
      await vi.advanceTimersByTimeAsync(500);
      await done;

      expect(mockPatch).toHaveBeenCalledExactlyOnceWith(
        expect.objectContaining({ body: patchBody("Running") }),
      );
      expect(sandbox.podIP).toBe("10.0.0.9");
      expect(mockCreateStrategy).toHaveBeenLastCalledWith(
        expect.anything(),
        expect.objectContaining({ podIP: "10.0.0.9" }),
      );
    });

    it("leaves a running Sandbox alone when its pod IP is unchanged", async () => {
      const running = sandboxObj({
        mode: "Running",
        conditions: [cond("Ready", 1)],
        podIPs: ["10.0.0.1"],
      });
      mockGet.mockResolvedValue(running);
      const sandbox = new Sandbox(createInit());
      mockCreateStrategy.mockClear();

      await sandbox.resume();

      expect(mockPatch).not.toHaveBeenCalled();
      expect(mockCreateStrategy).not.toHaveBeenCalled();
    });

    it("rebuilds the connection when a running Sandbox has a new pod IP", async () => {
      mockGet.mockResolvedValue(
        sandboxObj({
          mode: "Running",
          conditions: [cond("Ready", 1)],
          podIPs: ["10.0.0.5"],
        }),
      );
      const sandbox = new Sandbox(createInit());

      await sandbox.resume();

      expect(mockPatch).not.toHaveBeenCalled();
      expect(sandbox.podIP).toBe("10.0.0.5");
    });

    it("runs a suspend() and resume() issued together in order", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Suspended",
            generation: 2,
            conditions: [cond("Suspended", 2)],
          }),
        )
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Suspended",
            generation: 2,
            conditions: [cond("Suspended", 2)],
          }),
        )
        .mockResolvedValue(
          sandboxObj({
            mode: "Running",
            generation: 3,
            conditions: [cond("Ready", 3)],
          }),
        );
      mockPatch
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended", generation: 2 }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Running", generation: 3 }));
      const sandbox = new Sandbox(createInit());

      const both = Promise.all([sandbox.suspend(), sandbox.resume()]);
      await vi.advanceTimersByTimeAsync(1000);
      await both;

      expect(mockPatch.mock.calls.map((c) => c[0].body[1].value)).toEqual([
        "Suspended",
        "Running",
      ]);
    });
  });

  describe("connection", () => {
    let server: http.Server;
    let baseUrl: string;

    beforeEach(async () => {
      server = http.createServer((_req, res) => {
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify({ status: "ok", uptime_seconds: 1 }));
      });
      await new Promise<void>((resolve) =>
        server.listen(0, "127.0.0.1", resolve),
      );
      baseUrl = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
    });

    afterEach(async () => {
      await new Promise<void>((resolve) => server.close(() => resolve()));
    });

    it("closes the transport on suspend() and reconnects through a new strategy after resume()", async () => {
      const transports: Array<{ close: ReturnType<typeof vi.fn> }> = [];
      mockCreateStrategy.mockImplementation(
        (connectivity: string, target: { podIP: string }) => ({
          connectivity,
          open: vi.fn(async () => {
            const transport = {
              restBaseUrl: baseUrl,
              grpcBaseUrl: baseUrl,
              terminalError: () => undefined,
              close: vi.fn().mockResolvedValue(undefined),
              podIP: target.podIP,
            };
            transports.push(transport);
            return transport;
          }),
        }),
      );
      const sandbox = new Sandbox(createInit());
      await sandbox.health();
      expect(transports).toHaveLength(1);

      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(
          sandboxObj({ mode: "Suspended", conditions: [cond("Suspended", 2)] }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Suspended", generation: 2 }),
      );
      await sandbox.suspend();
      expect(transports[0].close).toHaveBeenCalledTimes(1);

      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended", generation: 2 }))
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Running",
            generation: 3,
            conditions: [cond("Ready", 3)],
            podIPs: ["10.0.0.9"],
          }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Running", generation: 3 }),
      );
      await sandbox.resume();
      await sandbox.health();

      expect(transports).toHaveLength(2);
      expect((transports[1] as { podIP?: string }).podIP).toBe("10.0.0.9");
    });

    it("discards a connect that was still pending when suspend() ran", async () => {
      const transports: Array<{ close: ReturnType<typeof vi.fn> }> = [];
      let release: () => void = () => {};
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      mockCreateStrategy.mockImplementation((connectivity: string) => ({
        connectivity,
        open: vi.fn(async () => {
          // Only the first connect is held open.
          if (transports.length === 0) {
            transports.push({ close: vi.fn().mockResolvedValue(undefined) });
            await gate;
            return {
              ...transports[0],
              restBaseUrl: baseUrl,
              grpcBaseUrl: baseUrl,
            };
          }
          throw new Error("unexpected connect");
        }),
      }));
      const sandbox = new Sandbox(createInit());
      const pending = sandbox.health().catch((e) => e);
      await vi.waitFor(() => expect(transports).toHaveLength(1));

      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(
          sandboxObj({ mode: "Suspended", conditions: [cond("Suspended", 2)] }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Suspended", generation: 2 }),
      );
      await sandbox.suspend();
      release();

      // The stale connect is torn down and its waiter fails instead of reusing it.
      expect(await pending).toBeInstanceOf(Error);
      expect(transports[0].close).toHaveBeenCalled();
    });
  });
});
