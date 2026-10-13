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

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// ---------- hoisted mock fns ----------

const { mockDeleteNamespacedCustomObject, mockGetNamespacedCustomObject } =
  vi.hoisted(() => ({
    mockDeleteNamespacedCustomObject: vi.fn(),
    mockGetNamespacedCustomObject: vi.fn(),
  }));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  const CustomObjectsApi = vi.fn();
  return { CustomObjectsApi };
});

// ---------- import SUT after mocks ----------

import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  SANDBOX_API_GROUP,
  SANDBOX_API_VERSION,
  SANDBOX_PLURAL_NAME,
} from "../constants.js";
import { SandboxError } from "../exceptions.js";
import type { SandboxInit } from "../sandbox.js";
import {
  findReadyCondition,
  normalizeSandboxdOptions,
  raceWithTimeout,
  readConditions,
  Sandbox,
} from "../sandbox.js";

// ---------- helpers ----------

function makeMockCustomObjectsApi() {
  return {
    deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
    getNamespacedCustomObject: mockGetNamespacedCustomObject,
  } as unknown as import("@kubernetes/client-node").CustomObjectsApi;
}

function makeMockKubeConfig() {
  return {} as unknown as import("@kubernetes/client-node").KubeConfig;
}

function makeFakeTracingManager() {
  return {
    endLifecycleSpan: vi.fn(),
  } as unknown as import("../trace-manager.js").TracerManager;
}

function createTestInit(overrides: Partial<SandboxInit> = {}): SandboxInit {
  return {
    claimName: "test-claim",
    sandboxName: "test-sandbox",
    podName: "test-pod",
    namespace: "default",
    customObjectsApi: makeMockCustomObjectsApi(),
    kubeConfig: makeMockKubeConfig(),
    sandboxdOptions: normalizeSandboxdOptions(),
    tracingManager: null,
    traceServiceName: "test-service",
    ...overrides,
  };
}

// ---------- tests ----------

describe("Sandbox", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe("identity", () => {
    it("exposes the claim / sandbox / pod identifiers", () => {
      const sandbox = new Sandbox(createTestInit());
      expect(sandbox.claimName).toBe("test-claim");
      expect(sandbox.sandboxName).toBe("test-sandbox");
      expect(sandbox.podName).toBe("test-pod");
      expect(sandbox.namespace).toBe("default");
    });
  });

  describe("isActive", () => {
    it("returns true after construction", () => {
      const sandbox = new Sandbox(createTestInit());
      expect(sandbox.isActive).toBe(true);
    });

    it("returns false after close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = new Sandbox(createTestInit());
      await sandbox.close();
      expect(sandbox.isActive).toBe(false);
    });

    it("returns false after closeLocal()", async () => {
      const sandbox = new Sandbox(createTestInit());
      await sandbox.closeLocal();
      expect(sandbox.isActive).toBe(false);
    });
  });

  describe("closeLocal()", () => {
    it("marks the handle inactive WITHOUT deleting the SandboxClaim", async () => {
      const sandbox = new Sandbox(createTestInit());
      await sandbox.closeLocal();
      expect(sandbox.isActive).toBe(false);
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });

    it("ends the tracing lifecycle span when a tracing manager is present", async () => {
      const tracingManager = makeFakeTracingManager();
      const sandbox = new Sandbox(createTestInit({ tracingManager }));
      await sandbox.closeLocal();
      expect(tracingManager.endLifecycleSpan).toHaveBeenCalledOnce();
    });

    it("is idempotent: a second call does not call K8s", async () => {
      const sandbox = new Sandbox(createTestInit());
      await sandbox.closeLocal();
      await sandbox.closeLocal();
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });
  });

  describe("close()", () => {
    it("deletes the SandboxClaim with the claim API coordinates", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = new Sandbox(createTestInit());

      await sandbox.close();

      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
      const args = mockDeleteNamespacedCustomObject.mock.calls[0][0];
      expect(args.group).toBe(CLAIM_API_GROUP);
      expect(args.version).toBe(CLAIM_API_VERSION);
      expect(args.plural).toBe(CLAIM_PLURAL_NAME);
      expect(args.namespace).toBe("default");
      expect(args.name).toBe("test-claim");
    });

    it("ends the tracing lifecycle span", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const tracingManager = makeFakeTracingManager();
      const sandbox = new Sandbox(createTestInit({ tracingManager }));

      await sandbox.close();

      expect(tracingManager.endLifecycleSpan).toHaveBeenCalledOnce();
    });

    it("does not throw when the claim is already gone (404)", async () => {
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("not found"), { code: 404 }),
      );
      const sandbox = new Sandbox(createTestInit());
      await expect(sandbox.close()).resolves.toBeUndefined();
    });

    it("rejects with SandboxError (preserving the cause) on a non-404 delete error", async () => {
      const cause = new Error("connection refused");
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(cause);
      const sandbox = new Sandbox(createTestInit());

      const err = await sandbox.close().catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect((err as Error).cause).toBe(cause);
      // The handle is still marked closed so a retry does not double-drain.
      expect(sandbox.isActive).toBe(false);
    });

    it("clears cleanup timers when close completes before the timeout", async () => {
      vi.useFakeTimers();
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

      const sandbox = new Sandbox(createTestInit());
      await sandbox.close();

      expect(vi.getTimerCount()).toBe(0);
    });

    it("rejects (bounded by the cleanup timeout) if the K8s delete never resolves", async () => {
      vi.useFakeTimers();
      mockDeleteNamespacedCustomObject.mockReturnValueOnce(
        new Promise(() => {}),
      );

      const sandbox = new Sandbox(createTestInit());
      const closed = sandbox.close();
      const assertion = expect(closed).rejects.toBeInstanceOf(SandboxError);
      await vi.runAllTimersAsync();
      await assertion;

      expect(sandbox.isActive).toBe(false);
    });
  });

  describe("status()", () => {
    const sandboxWithConditions = (
      conditions: Array<Record<string, string>> | undefined,
    ) => ({ status: conditions === undefined ? {} : { conditions } });

    it("reads the Sandbox object with the Sandbox API coordinates", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithConditions([]),
      );

      await new Sandbox(createTestInit()).status();

      expect(mockGetNamespacedCustomObject).toHaveBeenCalledExactlyOnceWith({
        group: SANDBOX_API_GROUP,
        version: SANDBOX_API_VERSION,
        namespace: "default",
        plural: SANDBOX_PLURAL_NAME,
        name: "test-sandbox",
      });
    });

    it("reports SandboxReady with the condition message", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithConditions([
          { type: "Ready", status: "True", message: "Pod is ready" },
        ]),
      );

      await expect(new Sandbox(createTestInit()).status()).resolves.toEqual({
        status: "SandboxReady",
        message: "Pod is ready",
      });
    });

    it("reports SandboxNotReady with the condition message", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithConditions([
          { type: "Ready", status: "False", message: "Pod is pending" },
        ]),
      );

      await expect(new Sandbox(createTestInit()).status()).resolves.toEqual({
        status: "SandboxNotReady",
        message: "Pod is pending",
      });
    });

    it("reports SandboxNotReady with an empty message when the condition has none", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithConditions([{ type: "Ready", status: "False" }]),
      );

      const result = await new Sandbox(createTestInit()).status();
      expect(result).toEqual({ status: "SandboxNotReady", message: "" });
    });

    it.each([
      ["no conditions", []],
      ["no Ready condition", [{ type: "Other", status: "True" }]],
      ["no status block", undefined],
    ])("reports SandboxNotReady when there are %s", async (_name, conditions) => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithConditions(conditions),
      );

      const result = await new Sandbox(createTestInit()).status();
      expect(result.status).toBe("SandboxNotReady");
    });

    it("reports SandboxNotFound on a 404", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("not found"), { code: 404 }),
      );

      const result = await new Sandbox(createTestInit()).status();
      expect(result.status).toBe("SandboxNotFound");
    });

    it("rejects with SandboxError (preserving the cause) on a non-404 error", async () => {
      const cause = new Error("connection refused");
      mockGetNamespacedCustomObject.mockRejectedValueOnce(cause);

      const err = await new Sandbox(createTestInit())
        .status()
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect((err as Error).cause).toBe(cause);
    });

    it("still works after close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("not found"), { statusCode: 404 }),
      );
      const sandbox = new Sandbox(createTestInit());
      await sandbox.close();

      const result = await sandbox.status();
      expect(result.status).toBe("SandboxNotFound");
    });
  });

  describe("getPodIP()", () => {
    const sandboxWithPodIPs = (podIPs: unknown) => ({ status: { podIPs } });

    it("reads the Sandbox object with the Sandbox API coordinates", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithPodIPs([{ ip: "10.0.0.1" }]),
      );

      await new Sandbox(createTestInit()).getPodIP();

      expect(mockGetNamespacedCustomObject).toHaveBeenCalledExactlyOnceWith({
        group: SANDBOX_API_GROUP,
        version: SANDBOX_API_VERSION,
        namespace: "default",
        plural: SANDBOX_PLURAL_NAME,
        name: "test-sandbox",
      });
    });

    it.each([
      ["IPv4 only", ["10.0.0.1"], "10.0.0.1"],
      ["dual-stack with IPv6 first", ["fd00::1", "10.0.0.2"], "10.0.0.2"],
      ["IPv4-mapped IPv6", ["::ffff:10.0.0.3"], "10.0.0.3"],
      ["IPv6 only", ["FD00::4"], "fd00::4"],
    ])("selects the pod IP for %s", async (_name, podIPs, want) => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(
        sandboxWithPodIPs(podIPs),
      );

      await expect(new Sandbox(createTestInit()).getPodIP()).resolves.toBe(
        want,
      );
    });

    it.each([
      ["an empty podIPs list", { status: { podIPs: [] } }],
      ["only invalid podIPs", { status: { podIPs: ["bogus"] } }],
      ["no podIPs", { status: {} }],
      ["no status block", {}],
    ])("resolves undefined for %s", async (_name, obj) => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce(obj);

      await expect(
        new Sandbox(createTestInit()).getPodIP(),
      ).resolves.toBeUndefined();
    });

    it("resolves undefined on a 404", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("not found"), { code: 404 }),
      );

      await expect(
        new Sandbox(createTestInit()).getPodIP(),
      ).resolves.toBeUndefined();
    });

    it("rejects with SandboxError (preserving the cause) on a non-404 error", async () => {
      const cause = Object.assign(new Error("boom"), { code: 500 });
      mockGetNamespacedCustomObject.mockRejectedValueOnce(cause);

      const err = await new Sandbox(createTestInit())
        .getPodIP()
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect((err as Error).cause).toBe(cause);
    });

    it("queries Kubernetes on every call instead of caching", async () => {
      mockGetNamespacedCustomObject
        .mockResolvedValueOnce(sandboxWithPodIPs(["10.0.0.1"]))
        .mockResolvedValueOnce(sandboxWithPodIPs(["10.0.0.9"]));
      const sandbox = new Sandbox(createTestInit());

      await expect(sandbox.getPodIP()).resolves.toBe("10.0.0.1");
      await expect(sandbox.getPodIP()).resolves.toBe("10.0.0.9");
      expect(mockGetNamespacedCustomObject).toHaveBeenCalledTimes(2);
    });
  });

  describe("[Symbol.asyncDispose]()", () => {
    it("closes the handle and deletes the claim", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = new Sandbox(createTestInit());

      await sandbox[Symbol.asyncDispose]();

      expect(sandbox.isActive).toBe(false);
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
    });
  });
});

describe("raceWithTimeout", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("returns the operation result when it settles first", async () => {
    const result = await raceWithTimeout(
      Promise.resolve("done"),
      1000,
      () => "timeout",
    );
    expect(result).toBe("done");
  });

  it("returns the onTimeout value when the operation is slower than the timeout", async () => {
    vi.useFakeTimers();
    const slow = new Promise<string>((resolve) =>
      setTimeout(() => resolve("done"), 10_000),
    );
    const raced = raceWithTimeout(slow, 100, () => "timeout");
    await vi.advanceTimersByTimeAsync(200);
    expect(await raced).toBe("timeout");
  });

  it("propagates an error thrown by onTimeout", async () => {
    vi.useFakeTimers();
    const raced = raceWithTimeout(new Promise<string>(() => {}), 100, () => {
      throw new Error("deadline exceeded");
    });
    const assertion = expect(raced).rejects.toThrow("deadline exceeded");
    await vi.advanceTimersByTimeAsync(200);
    await assertion;
  });

  it("clears the timeout timer once the operation settles", async () => {
    vi.useFakeTimers();
    await raceWithTimeout(Promise.resolve("done"), 1000, () => "timeout");
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe("readConditions / findReadyCondition", () => {
  it("returns no conditions for a missing object, status or conditions", () => {
    expect(readConditions(undefined)).toEqual([]);
    expect(readConditions({})).toEqual([]);
    expect(readConditions({ status: {} })).toEqual([]);
  });

  it("returns the conditions as reported", () => {
    const conditions = [{ type: "Ready", status: "True" }];
    expect(readConditions({ status: { conditions } })).toBe(conditions);
  });

  it("finds the Ready condition among others", () => {
    const ready = { type: "Ready", status: "False", reason: "Pending" };
    const obj = { status: { conditions: [{ type: "Other" }, ready] } };
    expect(findReadyCondition(obj)).toBe(ready);
  });

  it("returns undefined when there is no Ready condition", () => {
    expect(findReadyCondition(undefined)).toBeUndefined();
    expect(
      findReadyCondition({ status: { conditions: [{ type: "Other" }] } }),
    ).toBeUndefined();
  });
});
