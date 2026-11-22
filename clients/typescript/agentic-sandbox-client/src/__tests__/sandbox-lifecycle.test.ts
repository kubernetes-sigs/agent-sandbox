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

const { mockGet, mockPatch, mockWatch, mockCreateStrategy } = vi.hoisted(
  () => ({
    mockGet: vi.fn(),
    mockPatch: vi.fn(),
    mockWatch: vi.fn(),
    mockCreateStrategy: vi.fn(),
  }),
);

vi.mock("@kubernetes/client-node", () => ({
  CustomObjectsApi: vi.fn(),
  // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
  Watch: vi.fn().mockImplementation(function () {
    return { watch: mockWatch };
  }),
}));
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
  podName?: string;
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
      ...(o.podName
        ? { annotations: { "agents.x-k8s.io/pod-name": o.podName } }
        : {}),
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
    podName: "test-sandbox",
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

type WatchEvent = (type: string, obj: unknown) => void;
type WatchDone = (err: unknown) => void;

/**
 * Makes the next watch stay open, and returns its event and done callbacks
 * once the Sandbox has started it.
 */
function openWatch() {
  const abort = new AbortController();
  const started = new Promise<{ event: WatchEvent; done: WatchDone }>(
    (resolve) => {
      mockWatch.mockImplementationOnce(
        async (_path, _params, event: WatchEvent, done: WatchDone) => {
          resolve({ event, done });
          return abort;
        },
      );
    },
  );
  return { started, abort };
}

describe("Sandbox suspend/resume", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Without a watch the wait polls, which most cases here exercise.
    mockWatch.mockReset();
    mockWatch.mockRejectedValue(new Error("watch is forbidden"));
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

    it("rejects when a same-name Sandbox replaced the first one between calls", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Running", uid: "other" }));
      const sandbox = new Sandbox(createInit());

      await expect(sandbox.isSuspended()).resolves.toBe(false);
      await expect(sandbox.isSuspended()).rejects.toThrow(/was replaced/);
    });

    it("rejects a replacement returned by an overlapping first read", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Running", uid: "other" }));
      const sandbox = new Sandbox(createInit());

      const [first, second] = await Promise.allSettled([
        sandbox.isSuspended(),
        sandbox.isSuspended(),
      ]);

      expect(first).toEqual({ status: "fulfilled", value: false });
      expect(second).toMatchObject({ status: "rejected" });
      expect(String((second as PromiseRejectedResult).reason)).toMatch(
        /was replaced/,
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

    it("does not patch a same-name Sandbox that replaced the first one between calls", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Running", uid: "other" }));
      const sandbox = new Sandbox(createInit());

      await sandbox.isSuspended();
      await expect(sandbox.suspend()).rejects.toThrow(/was replaced/);
      expect(mockPatch).not.toHaveBeenCalled();
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

    it("does not let a stalled request block the next call", async () => {
      mockGet
        .mockReturnValueOnce(new Promise(() => {}))
        .mockResolvedValue(
          sandboxObj({ mode: "Suspended", conditions: [cond("Suspended", 1)] }),
        );
      const sandbox = new Sandbox(createInit());
      const stalled = expect(
        sandbox.suspend({ timeoutMs: 1000 }),
      ).rejects.toThrow(SandboxTimeoutError);

      await vi.advanceTimersByTimeAsync(1500);
      await stalled;

      await sandbox.suspend({ timeoutMs: 1000 });
    });
    it("waits for a timed-out patch before the next call reads", async () => {
      let mode = "Running";
      let generation = 1;
      let landPatch = () => {};
      mockGet.mockImplementation(async () =>
        sandboxObj({
          mode,
          generation,
          conditions: [
            cond("Suspended", generation),
            cond("Ready", generation),
          ],
        }),
      );
      mockPatch
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              landPatch = () => {
                mode = "Suspended";
                generation = 2;
                resolve(sandboxObj({ mode, generation }));
              };
            }),
        )
        .mockImplementationOnce(async () => {
          mode = "Running";
          generation = 3;
          return sandboxObj({ mode, generation });
        });
      const sandbox = new Sandbox(createInit());
      const stalled = expect(
        sandbox.suspend({ timeoutMs: 1000 }),
      ).rejects.toThrow(SandboxTimeoutError);
      await vi.advanceTimersByTimeAsync(1500);
      await stalled;

      const resumed = sandbox.resume();
      await vi.advanceTimersByTimeAsync(1000);
      expect(mockGet).toHaveBeenCalledTimes(1);

      landPatch();
      await vi.advanceTimersByTimeAsync(1000);
      await resumed;

      expect(mockPatch.mock.calls.map((c) => c[0].body[1].value)).toEqual([
        "Suspended",
        "Running",
      ]);
    });
  });

  describe("watching the Sandbox", () => {
    const suspending = sandboxObj({ mode: "Suspended", generation: 2 });
    const suspended = sandboxObj({
      mode: "Suspended",
      generation: 2,
      conditions: [cond("Suspended", 2)],
    });

    beforeEach(() => {
      mockPatch.mockResolvedValue(suspending);
    });

    it("returns on the watch event that reports Suspended, without another GET", async () => {
      mockGet.mockResolvedValueOnce(sandboxObj({ mode: "Running" }));
      mockGet.mockResolvedValueOnce({
        ...suspending,
        metadata: { ...suspending.metadata, resourceVersion: "42" },
      });
      const watch = openWatch();

      const done = new Sandbox(createInit()).suspend();
      const { event } = await watch.started;
      // An event that is not there yet keeps the watch open.
      event("MODIFIED", suspending);
      event("MODIFIED", suspended);
      await done;

      expect(mockGet).toHaveBeenCalledTimes(2);
      expect(mockWatch).toHaveBeenCalledExactlyOnceWith(
        "/apis/agents.x-k8s.io/v1beta1/namespaces/default/sandboxes",
        { fieldSelector: "metadata.name=test-sandbox", resourceVersion: "42" },
        expect.any(Function),
        expect.any(Function),
      );
      expect(watch.abort.signal.aborted).toBe(true);
    });

    it.each([
      ["a DELETED event", "DELETED", suspending],
      ["an event for another UID", "MODIFIED", sandboxObj({ uid: "other" })],
      ["a terminating event", "MODIFIED", sandboxObj({ deleting: true })],
    ])("reads the Sandbox again after %s and reports what the GET finds", async (_name, type, obj) => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(suspending)
        .mockRejectedValueOnce({ code: 404 });
      const watch = openWatch();

      const done = expect(new Sandbox(createInit()).suspend()).rejects.toThrow(
        SandboxNotFoundError,
      );
      (await watch.started).event(type, obj);
      await done;

      expect(mockGet).toHaveBeenCalledTimes(3);
    });

    it("watches again after the API server closes the watch", async () => {
      vi.useFakeTimers();
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValue(suspending);
      const first = openWatch();
      const second = openWatch();

      const done = new Sandbox(createInit()).suspend();
      (await first.started).done(null);
      await vi.advanceTimersByTimeAsync(500);
      (await second.started).event("MODIFIED", suspended);
      await done;

      expect(mockWatch).toHaveBeenCalledTimes(2);
    });

    it("polls for the rest of the call once the watch fails", async () => {
      vi.useFakeTimers();
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValueOnce(suspending)
        .mockResolvedValueOnce(suspending)
        .mockResolvedValueOnce(suspended);
      const watch = openWatch();

      const done = new Sandbox(createInit()).suspend();
      (await watch.started).done(
        Object.assign(new Error("forbidden"), { statusCode: 403 }),
      );
      await vi.advanceTimersByTimeAsync(1000);
      await done;

      expect(mockWatch).toHaveBeenCalledTimes(1);
      expect(mockGet).toHaveBeenCalledTimes(4);
    });

    it("ends the watch when the caller aborts", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Running" }))
        .mockResolvedValue(suspending);
      const watch = openWatch();
      const controller = new AbortController();
      const reason = new Error("caller gave up");

      const done = expect(
        new Sandbox(createInit()).suspend({ signal: controller.signal }),
      ).rejects.toBe(reason);
      await watch.started;
      controller.abort(reason);
      await done;

      expect(watch.abort.signal.aborted).toBe(true);
    });

    it("resume() adopts the pod IP from the watch event that reports Ready", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended" }))
        .mockResolvedValueOnce(sandboxObj({ mode: "Running", generation: 2 }));
      mockPatch.mockResolvedValue(
        sandboxObj({ mode: "Running", generation: 2 }),
      );
      const watch = openWatch();
      const sandbox = new Sandbox(createInit());

      const done = sandbox.resume();
      (await watch.started).event(
        "MODIFIED",
        sandboxObj({
          mode: "Running",
          generation: 2,
          podIPs: ["10.0.0.9"],
          conditions: [cond("Ready", 2)],
        }),
      );
      await done;

      expect(sandbox.podIP).toBe("10.0.0.9");
      expect(mockGet).toHaveBeenCalledTimes(2);
    });
  });

  describe("abort listeners", () => {
    it("abortable() and sleep() release their listeners once settled", async () => {
      vi.useFakeTimers();
      const sb = new Sandbox(createInit()) as unknown as {
        abortable: (s: AbortSignal, p: Promise<unknown>) => Promise<unknown>;
        sleep: (ms: number, s: AbortSignal) => Promise<void>;
      };
      const signal = new AbortController().signal;
      const added = vi.spyOn(signal, "addEventListener");
      const removed = vi.spyOn(signal, "removeEventListener");

      await sb.abortable(signal, Promise.resolve(1));
      await expect(
        sb.abortable(signal, Promise.reject(new Error("x"))),
      ).rejects.toThrow("x");
      const sleeping = sb.sleep(50, signal);
      await vi.advanceTimersByTimeAsync(50);
      await sleeping;

      expect(added).toHaveBeenCalledTimes(3);
      expect(removed).toHaveBeenCalledTimes(3);
    });

    it("abortable() observes a promise it rejects early for an aborted signal", async () => {
      const sb = new Sandbox(createInit()) as unknown as {
        abortable: (s: AbortSignal, p: Promise<unknown>) => Promise<unknown>;
      };
      const reason = new Error("stop");
      const unhandled: unknown[] = [];
      const onUnhandled = (e: unknown) => unhandled.push(e);
      process.on("unhandledRejection", onUnhandled);
      try {
        await expect(
          sb.abortable(
            AbortSignal.abort(reason),
            Promise.reject(new Error("late")),
          ),
        ).rejects.toBe(reason);
        await new Promise((resolve) => setImmediate(resolve));
      } finally {
        process.off("unhandledRejection", onUnhandled);
      }

      expect(unhandled).toEqual([]);
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

    it("targets the replacement pod when the pod-name annotation is cleared", async () => {
      mockGet
        .mockResolvedValueOnce(sandboxObj({ mode: "Suspended", generation: 2 }))
        .mockResolvedValueOnce(
          sandboxObj({
            mode: "Running",
            generation: 3,
            conditions: [cond("Ready", 3)],
            podIPs: ["10.0.0.1"],
          }),
        );
      mockPatch.mockResolvedValueOnce(
        sandboxObj({ mode: "Running", generation: 3 }),
      );
      const sandbox = new Sandbox(createInit({ podName: "adopted-pod" }));

      await sandbox.resume();

      expect(sandbox.podName).toBe("test-sandbox");
      expect(mockCreateStrategy).toHaveBeenLastCalledWith(
        expect.anything(),
        expect.objectContaining({ podName: "test-sandbox" }),
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

    it("adopts the new pod before the old connection finishes closing", async () => {
      let releaseClose: () => void = () => {};
      const closeGate = new Promise<void>((resolve) => {
        releaseClose = resolve;
      });
      const close = vi.fn(() => closeGate);
      mockCreateStrategy.mockImplementation((connectivity: string) => ({
        connectivity,
        open: vi.fn(async () => ({
          restBaseUrl: baseUrl,
          grpcBaseUrl: baseUrl,
          terminalError: () => undefined,
          close,
        })),
      }));
      const sandbox = new Sandbox(createInit());
      await sandbox.health();
      mockCreateStrategy.mockClear();

      mockGet.mockResolvedValue(
        sandboxObj({
          mode: "Running",
          conditions: [cond("Ready", 1)],
          podIPs: ["10.0.0.5"],
        }),
      );
      const resumed = sandbox.resume();
      await vi.waitFor(() => expect(close).toHaveBeenCalled());

      expect(mockCreateStrategy).toHaveBeenLastCalledWith(
        expect.anything(),
        expect.objectContaining({ podIP: "10.0.0.5" }),
      );
      releaseClose();
      await resumed;
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
