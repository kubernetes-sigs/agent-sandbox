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

// ---------- hoisted mock fns (accessible inside vi.mock factories) ----------

const {
  mockCreateNamespacedCustomObject,
  mockDeleteNamespacedCustomObject,
  mockGetNamespacedCustomObject,
  mockListNamespacedCustomObject,
  mockWatchFn,
  MockKubeConfig,
} = vi.hoisted(() => ({
  mockCreateNamespacedCustomObject: vi.fn(),
  mockDeleteNamespacedCustomObject: vi.fn(),
  mockGetNamespacedCustomObject: vi.fn(),
  mockListNamespacedCustomObject: vi.fn(),
  mockWatchFn: vi.fn(),
  MockKubeConfig: vi.fn(),
}));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  // Set the default implementation on MockKubeConfig (exposed via hoisted for per-test override)
  // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
  MockKubeConfig.mockImplementation(function () {
    return {
      loadFromDefault: vi.fn(),
      clusters: [{ name: "test-cluster" }],
      makeApiClient: vi.fn().mockReturnValue({
        createNamespacedCustomObject: mockCreateNamespacedCustomObject,
        deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
        getNamespacedCustomObject: mockGetNamespacedCustomObject,
        listNamespacedCustomObject: mockListNamespacedCustomObject,
      }),
    };
  });

  const CustomObjectsApi = vi.fn();

  // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
  const Watch = vi.fn().mockImplementation(function () {
    return { watch: mockWatchFn };
  });

  return { KubeConfig: MockKubeConfig, CustomObjectsApi, Watch };
});

// ---------- import SUT after mocks ----------

import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  POD_NAME_ANNOTATION,
} from "../constants.js";
import {
  SandboxClaimFailedError,
  SandboxError,
  SandboxNotFoundError,
  SandboxTemplateNotFoundError,
  SandboxWarmPoolNotFoundError,
} from "../exceptions.js";
import { Sandbox } from "../sandbox.js";
import { SandboxClient } from "../sandbox-client.js";

// ---------- helpers ----------

/**
 * Sets up two sequential Watch calls:
 *   1. SandboxClaim watch → resolves sandbox name
 *   2. Sandbox watch → becomes ready with optional pod annotation
 */
function mockSandboxReadyFlow(
  sandboxName: string,
  podAnnotation?: string,
): void {
  // First watch: SandboxClaim resolves actual sandbox name
  mockWatchFn.mockImplementationOnce(
    (
      _path: string,
      _query: unknown,
      callback: (type: string, obj: Record<string, unknown>) => void,
      _done: (err: unknown) => void,
    ) => {
      callback("MODIFIED", { status: { sandbox: { name: sandboxName } } });
      return Promise.resolve(new AbortController());
    },
  );

  // Second watch: Sandbox becomes Ready
  mockWatchFn.mockImplementationOnce(
    (
      _path: string,
      _query: unknown,
      callback: (type: string, obj: Record<string, unknown>) => void,
      _done: (err: unknown) => void,
    ) => {
      callback("MODIFIED", {
        metadata: {
          name: sandboxName,
          annotations: podAnnotation
            ? { [POD_NAME_ANNOTATION]: podAnnotation }
            : {},
        },
        status: { conditions: [{ type: "Ready", status: "True" }] },
      });
      return Promise.resolve(new AbortController());
    },
  );
}

// ---------- tests ----------

describe("SandboxClient (registry)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Reset GET and Watch mocks completely so that persistent implementations
    // (mockResolvedValue / mockImplementation) set in one test don't leak into
    // subsequent tests via the initial-GET or watch paths.
    mockGetNamespacedCustomObject.mockReset();
    mockWatchFn.mockReset();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // ===== constructor =====

  describe("constructor", () => {
    it("accepts empty options with sane defaults", () => {
      const client = new SandboxClient();
      expect(client).toBeInstanceOf(SandboxClient);
    });

    it("accepts all options without throwing", () => {
      const client = new SandboxClient({
        namespace: "prod",
        sandboxReadyTimeout: 60,
        enableTracing: false,
        traceServiceName: "my-service",
      });
      expect(client).toBeInstanceOf(SandboxClient);
    });
  });

  // ===== constructor validation =====

  describe("constructor validation", () => {
    it("throws SandboxError for sandboxReadyTimeout 0", () => {
      expect(() => new SandboxClient({ sandboxReadyTimeout: 0 })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for sandboxReadyTimeout negative", () => {
      expect(() => new SandboxClient({ sandboxReadyTimeout: -1 })).toThrow(
        SandboxError,
      );
    });

    it("accepts positive fractional timeout", () => {
      expect(
        () => new SandboxClient({ sandboxReadyTimeout: 0.1 }),
      ).not.toThrow();
    });

    it("throws SandboxError for empty namespace", () => {
      expect(() => new SandboxClient({ namespace: "" })).toThrow(SandboxError);
    });
  });

  // ===== createSandbox =====

  describe("createSandbox()", () => {
    it("full flow: creates claim, watches, constructs Sandbox, registers", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("my-sandbox", "my-pod-0");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("test-template", "default");

      expect(sandbox).toBeInstanceOf(Sandbox);
      expect(sandbox.isActive).toBe(true);
      expect(sandbox.claimName).toMatch(/^sandbox-claim-/);
      expect(sandbox.sandboxName).toBe("my-sandbox");
      expect(sandbox.podName).toBe("my-pod-0");
      expect(sandbox.namespace).toBe("default");

      // Verify claim was created
      expect(mockCreateNamespacedCustomObject).toHaveBeenCalledOnce();
      const createArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(createArgs.group).toBe(CLAIM_API_GROUP);
      expect(createArgs.version).toBe(CLAIM_API_VERSION);
      expect(createArgs.plural).toBe(CLAIM_PLURAL_NAME);
      expect(createArgs.namespace).toBe("default");
      expect(createArgs.body.spec.warmPoolRef.name).toBe("test-template");

      // Verify two watches were used
      expect(mockWatchFn).toHaveBeenCalledTimes(2);
    });

    it("uses default namespace from client options", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-ns");

      const client = new SandboxClient({
        namespace: "prod",
      });
      const sandbox = await client.createSandbox("tpl");

      expect(sandbox.namespace).toBe("prod");
      const createArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(createArgs.namespace).toBe("prod");
    });

    it("overrides namespace per-call", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-staging");

      const client = new SandboxClient({
        namespace: "prod",
      });
      const sandbox = await client.createSandbox("tpl", "staging");

      expect(sandbox.namespace).toBe("staging");
    });

    it("passes labels to the claim manifest", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-labeled");

      const client = new SandboxClient();
      await client.createSandbox("tpl", "default", {
        labels: { env: "test", team: "infra" },
      });

      const createArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(createArgs.body.metadata.labels).toEqual({
        env: "test",
        team: "infra",
      });
    });

    it("throws when warmpool is empty", async () => {
      const client = new SandboxClient();
      await expect(client.createSandbox("")).rejects.toThrow(
        "Warmpool name cannot be empty.",
      );
    });

    it.each([
      ["NaN", Number.NaN],
      ["zero", 0],
      ["negative", -1],
      ["Infinity", Number.POSITIVE_INFINITY],
    ])("rejects a non-positive per-call sandboxReadyTimeout (%s) before creating a claim", async (_label, timeout) => {
      const client = new SandboxClient();
      await expect(
        client.createSandbox("tpl", "default", {
          sandboxReadyTimeout: timeout,
        }),
      ).rejects.toThrow(SandboxError);
      expect(mockCreateNamespacedCustomObject).not.toHaveBeenCalled();
    });

    it("cleans up orphaned claim when sandbox watch fails", async () => {
      vi.useFakeTimers();
      try {
        mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
        mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

        // First watch: claim resolves
        mockWatchFn.mockImplementationOnce(
          (
            _path: string,
            _query: unknown,
            callback: (type: string, obj: Record<string, unknown>) => void,
          ) => {
            callback("MODIFIED", { status: { sandbox: { name: "s1" } } });
            return Promise.resolve(new AbortController());
          },
        );

        // Second watch: error
        mockWatchFn.mockImplementationOnce(
          (
            _path: string,
            _query: unknown,
            _callback: unknown,
            done: (err: unknown) => void,
          ) => {
            done(new Error("watch failed"));
            return Promise.resolve(new AbortController());
          },
        );

        const client = new SandboxClient();
        await expect(client.createSandbox("tpl")).rejects.toThrow(
          "watch failed",
        );

        // Orphaned claim should be deleted
        expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
        expect(vi.getTimerCount()).toBe(0);
      } finally {
        vi.useRealTimers();
      }
    });

    it("falls back to sandboxName when pod annotation is absent", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-xyz"); // no podAnnotation

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl");

      expect(sandbox.podName).toBe("sandbox-xyz");
      expect(sandbox.sandboxName).toBe("sandbox-xyz");
    });

    it("registers sandbox in the registry", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-reg");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl");

      const active = client.listActiveSandboxes();
      expect(active).toHaveLength(1);
      expect(active[0].claimName).toBe(sandbox.claimName);
    });

    it("re-raises original error even when rollback deletion fails", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // First watch: claim resolves sandbox name
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", { status: { sandbox: { name: "s-rb" } } });
          return Promise.resolve(new AbortController());
        },
      );

      // Second watch: error — triggers rollback
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          _callback: unknown,
          done: (err: unknown) => void,
        ) => {
          done(new Error("watch failed"));
          return Promise.resolve(new AbortController());
        },
      );

      // Rollback deletion also fails
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        new Error("K8s API unavailable"),
      );

      const client = new SandboxClient();
      // Original error is re-raised; cleanup error is logged but not surfaced
      await expect(client.createSandbox("tpl")).rejects.toThrow("watch failed");
      // Cleanup was still attempted
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
    });

    it("does not delete the claim when creation fails with 409 AlreadyExists", async () => {
      mockCreateNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("AlreadyExists"), { code: 409 }),
      );

      const client = new SandboxClient();
      await expect(client.createSandbox("tpl")).rejects.toThrow(
        "AlreadyExists",
      );

      // The colliding claim belongs to someone else — must not be deleted.
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });

    // empty namespace string should be normalized to defaultNamespace
    it("normalizes empty namespace string to default namespace", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-ns-norm");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl", "");

      expect(sandbox.namespace).toBe("default");
      const createArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(createArgs.namespace).toBe("default");
    });
  });

  // ===== getSandbox =====

  describe("getSandbox()", () => {
    it("returns cached handle when still active", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-cache");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // createSandbox() makes 2 initial GET calls (resolveSandboxName + watchForSandboxReady).
      // Clear the count so the assertion only covers the getSandbox() call below.
      mockGetNamespacedCustomObject.mockClear();

      // Cache-hit validation: 1) claim re-GET, 2) underlying Sandbox CR re-GET
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.sandboxName },
      });

      const sandbox2 = await client.getSandbox(sandbox1.claimName);

      expect(sandbox2).toBe(sandbox1);
      expect(mockGetNamespacedCustomObject).toHaveBeenCalledTimes(2);
    });

    it("re-attaches when cached handle is inactive (closed)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-reattach");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");
      const claimName = sandbox1.claimName;

      await sandbox1.close(); // marks as inactive

      // Mock claim verification and watch for re-attach
      mockGetNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-reattach");

      const sandbox2 = await client.getSandbox(claimName);
      expect(sandbox2).not.toBe(sandbox1);
      expect(sandbox2.isActive).toBe(true);
      expect(sandbox2.claimName).toBe(claimName);
    });

    it("throws when claim not found in Kubernetes", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const client = new SandboxClient();
      await expect(client.getSandbox("nonexistent-claim")).rejects.toThrow(
        "SandboxClaim 'nonexistent-claim' not found",
      );
    });

    it("shares one attach between concurrent calls for the same claim", async () => {
      // Claim GET, sandbox-name resolution and readiness are all satisfied by
      // the initial GETs, so the attach needs no watch.
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: "shared-sandbox", annotations: {} },
        status: {
          sandbox: { name: "shared-sandbox" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      const client = new SandboxClient();
      const [first, second] = await Promise.all([
        client.getSandbox("shared-claim"),
        client.getSandbox("shared-claim"),
      ]);

      expect(second).toBe(first);
      // A single attach: claim verification + resolveSandboxName + readiness.
      expect(mockGetNamespacedCustomObject).toHaveBeenCalledTimes(3);
      // The registered handle is the one both callers hold.
      expect(client.listActiveSandboxes()).toEqual([
        { namespace: "default", claimName: "shared-claim" },
      ]);
    });

    it("joins an in-flight createSandbox() instead of attaching a second handle", async () => {
      // A caller that discovers the claim (e.g. via listAllSandboxes()) while
      // createSandbox() is still waiting for readiness must get the handle that
      // create registers — not a competing one that create's registry.set()
      // would then drop, leaving it active but untracked.
      let claimName = "";
      mockCreateNamespacedCustomObject.mockImplementationOnce(
        (args: { body: { metadata: { name: string } } }) => {
          claimName = args.body.metadata.name;
          return Promise.resolve({});
        },
      );

      // Initial GET of resolveSandboxName: claim not resolved yet, so the
      // provision parks on the watch until the test releases it.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });
      let releaseWatch: (() => void) | undefined;
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          releaseWatch = () =>
            callback("MODIFIED", {
              status: { sandbox: { name: "sandbox-joined" } },
            });
          return Promise.resolve(new AbortController());
        },
      );
      // Readiness is satisfied by the GET in watchForSandboxReady.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: "sandbox-joined", annotations: {} },
        status: { conditions: [{ type: "Ready", status: "True" }] },
      });

      const client = new SandboxClient();
      const creating = client.createSandbox("tpl");
      await vi.waitFor(() => expect(releaseWatch).toBeDefined());

      const joining = client.getSandbox(claimName);
      releaseWatch?.();

      const [created, joined] = await Promise.all([creating, joining]);
      expect(joined).toBe(created);
      // No second attach ran: only the provision's own two GETs were issued.
      expect(mockGetNamespacedCustomObject).toHaveBeenCalledTimes(2);
      expect(client.listActiveSandboxes()).toEqual([
        { namespace: "default", claimName },
      ]);
    });

    it("allows a new attach after an in-flight attach fails", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const client = new SandboxClient();
      await expect(client.getSandbox("retry-claim")).rejects.toThrow(
        SandboxNotFoundError,
      );

      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: "retry-sandbox", annotations: {} },
        status: {
          sandbox: { name: "retry-sandbox" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      const sandbox = await client.getSandbox("retry-claim");
      expect(sandbox.isActive).toBe(true);
    });

    // empty namespace string should be normalized to defaultNamespace
    it("normalizes empty namespace string to default namespace", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP 404"),
      );

      const client = new SandboxClient();
      await expect(client.getSandbox("some-claim", "")).rejects.toThrow();

      const callArgs = mockGetNamespacedCustomObject.mock.calls[0][0];
      expect(callArgs.namespace).toBe("default");
    });
  });

  // ===== listActiveSandboxes =====

  describe("listActiveSandboxes()", () => {
    it("returns active sandboxes and prunes closed ones", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      mockSandboxReadyFlow("sandbox-a");
      mockSandboxReadyFlow("sandbox-b");

      const client = new SandboxClient();
      const sb1 = await client.createSandbox("tpl");
      const sb2 = await client.createSandbox("tpl");

      expect(client.listActiveSandboxes()).toHaveLength(2);

      await sb1.close();
      const active = client.listActiveSandboxes();
      expect(active).toHaveLength(1);
      expect(active[0].claimName).toBe(sb2.claimName);
    });

    it("returns empty list when no sandboxes", () => {
      const client = new SandboxClient();
      expect(client.listActiveSandboxes()).toEqual([]);
    });
  });

  // ===== listAllSandboxes =====

  describe("listAllSandboxes()", () => {
    it("returns claim names from Kubernetes", async () => {
      mockListNamespacedCustomObject.mockResolvedValueOnce({
        items: [
          { metadata: { name: "sandbox-claim-aaa" } },
          { metadata: { name: "sandbox-claim-bbb" } },
        ],
      });

      const client = new SandboxClient();
      const names = await client.listAllSandboxes("default");

      expect(names).toEqual(["sandbox-claim-aaa", "sandbox-claim-bbb"]);
      expect(mockListNamespacedCustomObject).toHaveBeenCalledWith({
        group: CLAIM_API_GROUP,
        version: CLAIM_API_VERSION,
        namespace: "default",
        plural: CLAIM_PLURAL_NAME,
      });
    });

    it("returns empty array when no claims exist", async () => {
      mockListNamespacedCustomObject.mockResolvedValueOnce({ items: [] });
      const client = new SandboxClient();
      expect(await client.listAllSandboxes()).toEqual([]);
    });
  });

  // ===== deleteSandbox =====

  describe("deleteSandbox()", () => {
    it("closes tracked sandbox and removes from registry", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-del");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl");
      const claimName = sandbox.claimName;

      expect(client.listActiveSandboxes()).toHaveLength(1);

      await client.deleteSandbox(claimName);

      expect(sandbox.isActive).toBe(false);
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("deletes claim directly when sandbox is not tracked", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

      const client = new SandboxClient();
      await client.deleteSandbox("untracked-claim", "default");

      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
      const args = mockDeleteNamespacedCustomObject.mock.calls[0][0];
      expect(args.name).toBe("untracked-claim");
    });

    it("does not throw when claim is already 404", async () => {
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const client = new SandboxClient();
      await expect(
        client.deleteSandbox("missing-claim"),
      ).resolves.toBeUndefined();
    });

    it("surfaces a non-404 delete failure so the caller can retry", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-del-retry");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl");

      // First delete fails transiently...
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Service Unavailable"), { code: 503 }),
      );
      await expect(
        client.deleteSandbox(sandbox.claimName),
      ).rejects.toBeInstanceOf(SandboxError);

      // ...the handle was evicted, so a retry takes the untracked-delete path.
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      await expect(
        client.deleteSandbox(sandbox.claimName),
      ).resolves.toBeUndefined();
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledTimes(2);
    });
  });

  // ===== deleteAll =====

  describe("deleteAll()", () => {
    it("closes all tracked sandboxes", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      mockSandboxReadyFlow("sandbox-all-a");
      mockSandboxReadyFlow("sandbox-all-b");

      const client = new SandboxClient();
      const sb1 = await client.createSandbox("tpl");
      const sb2 = await client.createSandbox("tpl");

      expect(client.listActiveSandboxes()).toHaveLength(2);

      await client.deleteAll();

      expect(sb1.isActive).toBe(false);
      expect(sb2.isActive).toBe(false);
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("is idempotent when no sandboxes exist", async () => {
      const client = new SandboxClient();
      await expect(client.deleteAll()).resolves.toBeUndefined();
    });

    it("deletes the claim of a createSandbox() that is still in flight", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      // Claim exists but has not resolved a sandbox name yet.
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { resourceVersion: "1" },
        status: {},
      });

      let claimWatchCallback:
        | ((type: string, obj: Record<string, unknown>) => void)
        | undefined;
      mockWatchFn.mockImplementation(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          claimWatchCallback = callback;
          return Promise.resolve(new AbortController());
        },
      );
      mockDeleteNamespacedCustomObject.mockImplementation(() => {
        // The deletion reaches the pending watch, as it would in a cluster.
        claimWatchCallback?.("DELETED", {});
        return Promise.resolve({});
      });

      const client = new SandboxClient();
      const pending = client.createSandbox("tpl");
      await vi.waitFor(() => expect(claimWatchCallback).toBeDefined());

      await client.deleteAll();

      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledWith(
        expect.objectContaining({ plural: CLAIM_PLURAL_NAME }),
      );
      await expect(pending).rejects.toBeInstanceOf(SandboxError);
    });

    it("tears down a claim whose create landed after the sweep", async () => {
      let releaseCreate: (() => void) | undefined;
      const createLanded = new Promise<void>((resolve) => {
        releaseCreate = resolve;
      });
      mockCreateNamespacedCustomObject.mockImplementation(async () => {
        await createLanded;
        return {};
      });
      // The sweep runs before the apiserver has the claim...
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );
      // ...so the rollback inside createSandbox() has to remove it instead.
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      const client = new SandboxClient();
      const pending = client.createSandbox("tpl");
      await vi.waitFor(() =>
        expect(mockCreateNamespacedCustomObject).toHaveBeenCalled(),
      );

      const sweep = client.deleteAll();
      await vi.waitFor(() =>
        expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledTimes(1),
      );
      releaseCreate?.();

      await expect(pending).rejects.toBeInstanceOf(SandboxError);
      await sweep;
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledTimes(2);
    });

    it("discards a handle whose create resolved during the sweep", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { resourceVersion: "1" },
        status: {},
      });

      // The claim resolves a sandbox name right away; the Sandbox watch is held
      // open so readiness can be delivered while the sweep is in flight.
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: { sandbox: { name: "sandbox-race" } },
          });
          return Promise.resolve(new AbortController());
        },
      );
      let sandboxWatchCallback:
        | ((type: string, obj: Record<string, unknown>) => void)
        | undefined;
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          sandboxWatchCallback = callback;
          return Promise.resolve(new AbortController());
        },
      );

      let releaseDelete: (() => void) | undefined;
      const deleteLanded = new Promise<void>((resolve) => {
        releaseDelete = resolve;
      });
      mockDeleteNamespacedCustomObject.mockImplementation(async () => {
        await deleteLanded;
        return {};
      });

      const client = new SandboxClient();
      const pending = client.createSandbox("tpl");
      await vi.waitFor(() => expect(sandboxWatchCallback).toBeDefined());

      const sweep = client.deleteAll();
      await vi.waitFor(() =>
        expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledTimes(1),
      );

      // The sandbox turns ready before the claim deletion completes, so the
      // create registers a handle for a claim that is already being removed.
      sandboxWatchCallback?.("MODIFIED", {
        metadata: { name: "sandbox-race", annotations: {} },
        status: { conditions: [{ type: "Ready", status: "True" }] },
      });
      const sandbox = await pending;
      releaseDelete?.();
      await sweep;

      expect(sandbox.isActive).toBe(false);
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });
  });

  // ===== [Symbol.asyncDispose] =====

  describe("[Symbol.asyncDispose]()", () => {
    it("calls deleteAll()", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-dispose");

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("tpl");

      await client[Symbol.asyncDispose]();

      expect(sandbox.isActive).toBe(false);
    });
  });

  // ===== getSandbox() re-attach preserves the SandboxClaim =====

  describe("getSandbox() preserves SandboxClaim on re-attach", () => {
    it("does not delete the SandboxClaim when re-attaching a stale handle", async () => {
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: "test-claim", annotations: {} },
        status: {
          sandbox: { name: "test-sandbox" },
          sandboxRef: { name: "test-sandbox", namespace: "default" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      mockSandboxReadyFlow("test-sandbox");

      const client = new SandboxClient();

      // First createSandbox so we have something in the registry.
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = await client.createSandbox("tpl");
      const claimName = sandbox.claimName;
      const sandboxName = sandbox.sandboxName;

      // Close locally to mark it inactive (simulates a stale handle).
      await sandbox.closeLocal();
      expect(sandbox.isActive).toBe(false);

      // getSandbox should re-attach; set up watch mocks for the re-attach flow.
      mockSandboxReadyFlow(sandboxName);
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: claimName, annotations: {} },
        status: {
          sandbox: { name: sandboxName },
          sandboxRef: { name: sandboxName, namespace: "default" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      const reattached = await client.getSandbox(claimName);
      expect(reattached).toBeInstanceOf(Sandbox);
      expect(reattached.isActive).toBe(true);

      // The claim must NOT have been deleted during re-attachment.
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });
  });

  // ===== watch miss on already-ready claim =====

  describe("watch miss on already-ready claim", () => {
    it("createSandbox resolves when claim is already ready (initial GET needed)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // watch fires no events — simulates a resource that was ready before watch started
      mockWatchFn.mockImplementation(
        (_path: string, _query: unknown, _cb: unknown, _done: unknown) =>
          Promise.resolve(new AbortController()),
      );

      // GET returns an object whose status satisfies both:
      //   - resolveSandboxName:  status.sandbox.name is set
      //   - watchForSandboxReady: status.conditions includes Ready=True
      // Use Once×2 (not persistent mockResolvedValue) to avoid polluting later tests.
      const readyObject = {
        metadata: { name: "already-ready-sandbox", annotations: {} },
        status: {
          sandbox: { name: "already-ready-sandbox" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      };
      mockGetNamespacedCustomObject.mockResolvedValueOnce(readyObject);
      mockGetNamespacedCustomObject.mockResolvedValueOnce(readyObject);

      const client = new SandboxClient({
        sandboxReadyTimeout: 1, // 1s timeout — watch-only code would time out
      });

      await expect(client.createSandbox("tpl")).resolves.toBeInstanceOf(
        Sandbox,
      );
    }, 5_000);
  });

  // ===== watch resumes from the GET resourceVersion =====

  describe("watch starts from the preceding GET's resourceVersion", () => {
    // A Sandbox watch pass that immediately reports the resource Ready.
    const sandboxReadyWatch =
      (name: string) =>
      (
        _path: string,
        _query: unknown,
        callback: (type: string, obj: Record<string, unknown>) => void,
      ) => {
        callback("MODIFIED", {
          metadata: { name, annotations: {} },
          status: { conditions: [{ type: "Ready", status: "True" }] },
        });
        return Promise.resolve(new AbortController());
      };

    it("claim watch passes the claim's metadata.resourceVersion", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // GET claim: exists, has a resourceVersion, but no sandbox name yet.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { resourceVersion: "claim-rv-123" },
        status: {},
      });
      // Claim watch resolves the sandbox name...
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", { status: { sandbox: { name: "sb-rv" } } });
          return Promise.resolve(new AbortController());
        },
      );
      // ...then the sandbox watch reports Ready.
      mockWatchFn.mockImplementationOnce(sandboxReadyWatch("sb-rv"));

      const client = new SandboxClient();
      await client.createSandbox("tpl");

      const claimWatchQuery = mockWatchFn.mock.calls[0][1] as {
        resourceVersion?: string;
      };
      expect(claimWatchQuery.resourceVersion).toBe("claim-rv-123");
    });

    it("sandbox watch passes the Sandbox's metadata.resourceVersion", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // GET claim: already resolved to a sandbox name (no claim watch happens).
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { resourceVersion: "claim-rv" },
        status: { sandbox: { name: "sb-ready" } },
      });
      // GET sandbox: exists with a resourceVersion, not Ready yet.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: "sb-ready", resourceVersion: "sandbox-rv-456" },
        status: {},
      });
      mockWatchFn.mockImplementationOnce(sandboxReadyWatch("sb-ready"));

      const client = new SandboxClient();
      await client.createSandbox("tpl");

      // Only one watch was needed (the claim resolved via GET): the sandbox watch.
      const sandboxWatchQuery = mockWatchFn.mock.calls[0][1] as {
        resourceVersion?: string;
      };
      expect(sandboxWatchQuery.resourceVersion).toBe("sandbox-rv-456");
    });

    it("treats an ERROR watch event (stale resourceVersion) as a clean close and re-GETs", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // First GET: claim not resolved yet.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { resourceVersion: "stale" },
        status: {},
      });
      // First claim watch: apiserver rejects the resourceVersion with an ERROR event.
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("ERROR", { code: 410, message: "too old resource version" });
          return Promise.resolve(new AbortController());
        },
      );
      // Re-GET after the clean close: claim is now resolved.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { resourceVersion: "fresh" },
        status: { sandbox: { name: "sb-after-410" } },
      });
      mockWatchFn.mockImplementationOnce(sandboxReadyWatch("sb-after-410"));

      const client = new SandboxClient({ sandboxReadyTimeout: 2 });
      const sandbox = await client.createSandbox("tpl");
      expect(sandbox.sandboxName).toBe("sb-after-410");
    }, 5_000);
  });

  // ===== watch done(null) triggers re-list (not immediate hang) =====
  // done(null) now causes re-list + re-watch in a loop.
  // The loop terminates via SandboxTimeoutError when the budget is exhausted.

  describe("watch stream clean close triggers re-list, not hang", () => {
    it("resolveSandboxName times out (not hangs) when done(null) fires and re-list stays unresolved", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // watch immediately calls done(null) — clean close with no events
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          _cb: unknown,
          done: (err: unknown) => void,
        ) => {
          Promise.resolve().then(() => done(null));
          return Promise.resolve(new AbortController());
        },
      );
      // Subsequent watch passes never fire events → internal timer fires "closed"
      mockWatchFn.mockImplementation(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.resolve(new AbortController()),
      );

      const { SandboxTimeoutError } = await import("../exceptions.js");
      const client = new SandboxClient({
        sandboxReadyTimeout: 0.05, // 50 ms — exhausted after re-list + backoff
      });

      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);

    it("watchForSandboxReady times out (not hangs) when done(null) fires and sandbox stays not ready", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // First watch: claim resolves sandbox name normally
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: { sandbox: { name: "test-sandbox" } },
          });
          return Promise.resolve(new AbortController());
        },
      );

      // Second watch (watchForSandboxReady): done(null) — clean close, no ready event
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          _cb: unknown,
          done: (err: unknown) => void,
        ) => {
          Promise.resolve().then(() => done(null));
          return Promise.resolve(new AbortController());
        },
      );
      // Subsequent watch passes never fire events → internal timer fires "closed"
      mockWatchFn.mockImplementation(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.resolve(new AbortController()),
      );

      const { SandboxTimeoutError } = await import("../exceptions.js");
      const client = new SandboxClient({
        sandboxReadyTimeout: 0.05, // 50 ms
      });

      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);

    it("treats the client library's 30s TimeoutError as a clean close and re-GETs", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // GET claim: not resolved yet.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });
      // Claim watch: @kubernetes/client-node 2.x aborts the request after 30s
      // and reports it as a DOMException named "TimeoutError".
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          _cb: unknown,
          done: (err: unknown) => void,
        ) => {
          Promise.resolve().then(() =>
            done(
              new DOMException(
                "The operation was aborted due to timeout",
                "TimeoutError",
              ),
            ),
          );
          return Promise.resolve(new AbortController());
        },
      );
      // Re-GET after the clean close: claim is now resolved.
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        status: { sandbox: { name: "sb-after-timeout" } },
      });
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "sb-after-timeout", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient({ sandboxReadyTimeout: 2 });
      const sandbox = await client.createSandbox("tpl");
      expect(sandbox.sandboxName).toBe("sb-after-timeout");
    }, 5_000);
  });

  // ===== SandboxTimeoutError on timeout =====

  describe("timeout throws SandboxTimeoutError", () => {
    it("resolveSandboxName timeout throws SandboxTimeoutError (not SandboxNotFoundError)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // watch never fires any event → timeout
      mockWatchFn.mockImplementation(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.resolve(new AbortController()),
      );

      const client = new SandboxClient({
        sandboxReadyTimeout: 0.05, // 50 ms
      });

      const { SandboxTimeoutError } = await import("../exceptions.js");
      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);

    it("watchForSandboxReady timeout throws SandboxTimeoutError", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // First watch: claim resolves immediately
      mockWatchFn.mockImplementationOnce(
        (
          _p: string,
          _q: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: { sandbox: { name: "sb-timeout" } },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Second watch (watchForSandboxReady): never fires → timeout
      mockWatchFn.mockImplementation(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.resolve(new AbortController()),
      );

      const client = new SandboxClient({
        sandboxReadyTimeout: 0.05,
      });

      const { SandboxTimeoutError } = await import("../exceptions.js");
      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);
  });

  // ===== watch startup failure =====

  describe("watch startup failure propagates", () => {
    it("resolveSandboxName rejects immediately when watcher.watch() rejects", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // watch() Promise itself rejects (startup failure)
      mockWatchFn.mockImplementationOnce(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.reject(new Error("ECONNREFUSED")),
      );

      const client = new SandboxClient();
      await expect(client.createSandbox("tpl")).rejects.toThrow("ECONNREFUSED");
    });

    it("watchForSandboxReady rejects immediately when watcher.watch() rejects", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // First watch succeeds (resolve sandbox name)
      mockWatchFn.mockImplementationOnce(
        (
          _p: string,
          _q: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: { sandbox: { name: "sb-19" } },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Second watch (watchForSandboxReady) fails at startup
      mockWatchFn.mockImplementationOnce(
        (_p: string, _q: unknown, _cb: unknown, _done: unknown) =>
          Promise.reject(new Error("watch startup failed")),
      );

      const client = new SandboxClient();
      await expect(client.createSandbox("tpl")).rejects.toThrow(
        "watch startup failed",
      );
    });
  });

  // ===== KubeConfig fail-fast =====

  describe("KubeConfig validation", () => {
    it("throws SandboxError when clusters array is empty", () => {
      // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
      MockKubeConfig.mockImplementationOnce(function () {
        return {
          loadFromDefault: vi.fn(),
          clusters: [], // empty → no kubeconfig configured
          makeApiClient: vi.fn(),
        };
      });
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });

    it("throws SandboxError when clusters is undefined", () => {
      // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
      MockKubeConfig.mockImplementationOnce(function () {
        return {
          loadFromDefault: vi.fn(),
          clusters: undefined, // undefined → no kubeconfig configured
          makeApiClient: vi.fn(),
        };
      });
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });

    it("throws SandboxError when only cluster is localhost:8080 (loadFromDefault fallback)", () => {
      // biome-ignore lint/complexity/useArrowFunction: constructor mock requires function keyword
      MockKubeConfig.mockImplementationOnce(function () {
        return {
          loadFromDefault: vi.fn(),
          clusters: [{ name: "in-cluster", server: "http://localhost:8080" }],
          makeApiClient: vi.fn(),
        };
      });
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });
  });

  // ===== option validation =====

  describe("namespace DNS label validation", () => {
    it("throws SandboxError for namespace with uppercase letters", () => {
      expect(() => new SandboxClient({ namespace: "MyNamespace" })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for namespace exceeding 63 characters", () => {
      expect(() => new SandboxClient({ namespace: "a".repeat(64) })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for namespace starting with a hyphen", () => {
      expect(() => new SandboxClient({ namespace: "-bad-ns" })).toThrow(
        SandboxError,
      );
    });

    it("accepts valid lowercase namespace", () => {
      expect(
        () => new SandboxClient({ namespace: "my-namespace" }),
      ).not.toThrow();
    });
  });

  // ===== enableAutoCleanup idempotency =====

  describe("enableAutoCleanup()", () => {
    it("is idempotent: second call returns no-op and does not register duplicate handlers", () => {
      const client = new SandboxClient();
      const listenerCountBefore = process.listenerCount("SIGINT");

      const stop1 = client.enableAutoCleanup();
      const stop2 = client.enableAutoCleanup(); // should be no-op

      expect(process.listenerCount("SIGINT")).toBe(listenerCountBefore + 1);

      stop1(); // removes the real handler
      expect(process.listenerCount("SIGINT")).toBe(listenerCountBefore);

      // stop2 is no-op — calling it should not throw
      expect(() => stop2()).not.toThrow();
    });

    it("allows re-registration after stop()", () => {
      const client = new SandboxClient();
      const baseLine = process.listenerCount("SIGINT");

      const stop = client.enableAutoCleanup();
      expect(process.listenerCount("SIGINT")).toBe(baseLine + 1);
      stop();
      expect(process.listenerCount("SIGINT")).toBe(baseLine);

      // After stop(), a new call should register again
      const stop2 = client.enableAutoCleanup();
      expect(process.listenerCount("SIGINT")).toBe(baseLine + 1);
      stop2();
    });

    it("re-raises the signal only after every client finished cleaning up", async () => {
      const clientA = new SandboxClient();
      const clientB = new SandboxClient();

      // Hold clientA's cleanup open so clientB finishes first: the signal must
      // not be re-raised (terminating the process) until A is done too.
      let releaseA: (() => void) | undefined;
      const cleanupA = new Promise<void>((resolve) => {
        releaseA = resolve;
      });
      const deleteAllA = vi
        .spyOn(clientA, "deleteAll")
        .mockImplementation(() => cleanupA);
      const deleteAllB = vi
        .spyOn(clientB, "deleteAll")
        .mockResolvedValue(undefined);

      const killSpy = vi
        .spyOn(process, "kill")
        .mockImplementation(() => true as never);
      const baseLine = process.listenerCount("SIGINT");
      const stopA = clientA.enableAutoCleanup();
      const stopB = clientB.enableAutoCleanup();

      try {
        // Both clients share one handler, so the process is torn down once.
        expect(process.listenerCount("SIGINT")).toBe(baseLine + 1);

        process.emit("SIGINT", "SIGINT");

        await vi.waitFor(() => expect(deleteAllB).toHaveBeenCalledTimes(1));
        expect(deleteAllA).toHaveBeenCalledTimes(1);
        expect(killSpy).not.toHaveBeenCalled();

        releaseA?.();
        await vi.waitFor(() => expect(killSpy).toHaveBeenCalledTimes(1));
        expect(killSpy).toHaveBeenCalledWith(process.pid, "SIGINT");
      } finally {
        killSpy.mockRestore();
        deleteAllA.mockRestore();
        deleteAllB.mockRestore();
        stopA();
        stopB();
      }
    });
  });

  // ===== getSandbox() K8s re-validation on cache hit =====

  describe("getSandbox() K8s re-validation on cache hit", () => {
    it("evicts cached handle and throws when claim returns 404 on cache hit", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-evict");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // K8s GET returns 404 during cache-hit validation
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        "SandboxClaim",
      );
      // Registry evicted — no active sandboxes remain
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("evicts cached handle and throws SandboxNotFoundError when underlying Sandbox returns 404 on cache hit", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-underlying");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // 1st GET: claim verify succeeds with matching sandbox name
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      // 2nd GET: Sandbox CR returns 404
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        SandboxNotFoundError,
      );
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("keeps cached handle and throws SandboxError on non-404 Sandbox GET error", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-non404");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // 1st GET: claim verify succeeds with matching sandbox name
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      // 2nd GET: Sandbox CR returns 500 (transient API server error)
      const apiErr = Object.assign(
        new Error("HTTP 500 internal server error"),
        {
          code: 500,
        },
      );
      mockGetNamespacedCustomObject.mockRejectedValueOnce(apiErr);

      let caught: unknown;
      try {
        await client.getSandbox(sandbox1.claimName);
      } catch (e) {
        caught = e;
      }
      expect(caught).toBeInstanceOf(SandboxError);
      expect(caught).not.toBeInstanceOf(SandboxNotFoundError);

      // A transient failure proves nothing about the Sandbox, so the handle
      // stays active and usable: a retry that succeeds returns the same one.
      expect(sandbox1.isActive).toBe(true);
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.sandboxName },
      });
      await expect(client.getSandbox(sandbox1.claimName)).resolves.toBe(
        sandbox1,
      );
    });

    it("evicts cached handle when sandboxRef name has changed since creation", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-original");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // Cache-hit validation returns claim with a *different* sandbox name
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: {
          sandbox: { name: "sandbox-changed" },
          sandboxRef: { name: "sandbox-changed", namespace: "default" },
        },
      });

      // Set up re-attach watch flow for the new sandbox name
      mockSandboxReadyFlow("sandbox-changed");

      const sandbox2 = await client.getSandbox(sandbox1.claimName);
      expect(sandbox2.sandboxName).toBe("sandbox-changed");
    });
  });

  // ===== watch done(null) triggers re-list and re-watch =====

  describe("watch done(null) triggers re-list and re-watch", () => {
    it("createSandbox succeeds after done(null) if re-list finds resolved sandbox", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // Initial GET for resolveSandboxName: not yet resolved
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // First watch: done(null) immediately — clean close with no events
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          _cb: unknown,
          done: (err: unknown) => void,
        ) => {
          Promise.resolve().then(() => done(null));
          return Promise.resolve(new AbortController());
        },
      );

      // Re-list GET after done(null): claim is now resolved
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: "sandbox-relisted", annotations: {} },
        status: {
          sandbox: { name: "sandbox-relisted" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      // Second watch pass (watchForSandboxReady): sandbox already ready via GET
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: "sandbox-relisted", annotations: {} },
        status: {
          sandbox: { name: "sandbox-relisted" },
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      const client = new SandboxClient({
        sandboxReadyTimeout: 2,
      });

      // createSandbox eventually succeeds by re-listing after done(null).
      const sandbox = await client.createSandbox("tpl");
      expect(sandbox.sandboxName).toBe("sandbox-relisted");
    }, 5_000);
  });

  // ===== initial GET 404 → immediate SandboxNotFoundError, no watch =====

  describe("resolveSandboxName() 404 on initial GET", () => {
    it("throws SandboxNotFoundError immediately and never calls watch on 404", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // Initial GET for claim returns 404
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const { SandboxNotFoundError } = await import("../exceptions.js");
      const client = new SandboxClient();
      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxNotFoundError,
      );
      // Watch must NEVER be called — 404 must fail immediately
      expect(mockWatchFn).not.toHaveBeenCalled();
    }, 3_000);
  });

  // ===== non-404 K8s errors are not collapsed into SandboxNotFoundError =====

  describe("getSandbox() non-404 error discrimination", () => {
    it("throws SandboxError (not SandboxNotFoundError) on non-404 error during cache miss", async () => {
      const k8sErr = Object.assign(new Error("Service Unavailable"), {
        code: 503,
      });
      mockGetNamespacedCustomObject.mockRejectedValueOnce(k8sErr);

      const { SandboxError, SandboxNotFoundError } = await import(
        "../exceptions.js"
      );
      const client = new SandboxClient();
      const err = await client
        .getSandbox("some-claim")
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect(err).not.toBeInstanceOf(SandboxNotFoundError);
      // Original error preserved as cause
      expect((err as SandboxError).cause).toBe(k8sErr);
    });

    it("throws SandboxError (not SandboxNotFoundError) on non-404 error during cache hit", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-non404");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");

      // Cache-hit validation returns non-404 error
      const k8sErr = Object.assign(new Error("Internal Server Error"), {
        code: 500,
      });
      mockGetNamespacedCustomObject.mockRejectedValueOnce(k8sErr);

      const { SandboxError, SandboxNotFoundError } = await import(
        "../exceptions.js"
      );
      const err = await client
        .getSandbox(sandbox1.claimName)
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect(err).not.toBeInstanceOf(SandboxNotFoundError);
      expect((err as SandboxError).cause).toBe(k8sErr);
      // Handle kept in the registry: the error says nothing about the sandbox.
      expect(client.listActiveSandboxes()).toEqual([
        { namespace: "default", claimName: sandbox1.claimName },
      ]);
    });
  });

  // ===== SandboxWarmPoolNotFoundError / SandboxTemplateNotFoundError =====

  describe("claim condition error detection", () => {
    it("watch path: throws SandboxWarmPoolNotFoundError on WarmPoolNotFound condition", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // Initial GET returns no sandbox name yet
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // Watch fires a MODIFIED event with WarmPoolNotFound condition
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                {
                  type: "Ready",
                  status: "False",
                  reason: "WarmPoolNotFound",
                  message: "pool not found",
                },
              ],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      await expect(client.createSandbox("missing-pool")).rejects.toBeInstanceOf(
        SandboxWarmPoolNotFoundError,
      );
    });

    it("GET path: throws SandboxWarmPoolNotFoundError on WarmPoolNotFound condition", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // Initial GET returns WarmPoolNotFound condition
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        status: {
          conditions: [
            {
              type: "Ready",
              status: "False",
              reason: "WarmPoolNotFound",
              message: "pool not found",
            },
          ],
        },
      });

      const client = new SandboxClient();
      await expect(client.createSandbox("missing-pool")).rejects.toBeInstanceOf(
        SandboxWarmPoolNotFoundError,
      );
      // Watch must NOT have been called — error propagates from GET path
      expect(mockWatchFn).not.toHaveBeenCalled();
    });

    it("watch path: throws SandboxTemplateNotFoundError on Ready=False/TemplateNotFound condition", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                {
                  type: "Ready",
                  status: "False",
                  reason: "TemplateNotFound",
                  message: "template missing",
                },
              ],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      await expect(
        client.createSandbox("warmpool-with-bad-tpl"),
      ).rejects.toBeInstanceOf(SandboxTemplateNotFoundError);
    });

    it("GET path: throws SandboxTemplateNotFoundError on Ready=False/TemplateNotFound condition", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        status: {
          conditions: [
            {
              type: "Ready",
              status: "False",
              reason: "TemplateNotFound",
              message: "template missing",
            },
          ],
        },
      });

      const client = new SandboxClient();
      await expect(
        client.createSandbox("warmpool-with-bad-tpl"),
      ).rejects.toBeInstanceOf(SandboxTemplateNotFoundError);
      expect(mockWatchFn).not.toHaveBeenCalled();
    });

    it("does NOT throw for Ready=True/TemplateNotFound (condition status mismatch)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // Watch fires Ready=True with reason TemplateNotFound — should NOT error, just continue
      // Then immediately give the sandbox name
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                { type: "Ready", status: "True", reason: "TemplateNotFound" },
              ],
              sandbox: { name: "sandbox-ok" },
            },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Second watch: sandbox becomes ready
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "sandbox-ok", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("warmpool-ok");
      expect(sandbox.sandboxName).toBe("sandbox-ok");
    });

    it("does NOT throw for Ready=True/WarmPoolNotFound (condition status mismatch)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // Watch fires Ready=True with reason WarmPoolNotFound — should NOT error, just continue
      // Then immediately give the sandbox name
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                { type: "Ready", status: "True", reason: "WarmPoolNotFound" },
              ],
              sandbox: { name: "sandbox-ok" },
            },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Second watch: sandbox becomes ready
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "sandbox-ok", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("warmpool-ok");
      expect(sandbox.sandboxName).toBe("sandbox-ok");
    });

    it("does NOT throw for Synced=False/WarmPoolNotFound (condition type mismatch)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // Watch fires a condition of a different type (Synced) with reason WarmPoolNotFound —
      // should NOT error, just continue. Then immediately give the sandbox name.
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                {
                  type: "Synced",
                  status: "False",
                  reason: "WarmPoolNotFound",
                },
              ],
              sandbox: { name: "sandbox-ok" },
            },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Second watch: sandbox becomes ready
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "sandbox-ok", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("warmpool-ok");
      expect(sandbox.sandboxName).toBe("sandbox-ok");
    });

    it("GET path: throws SandboxClaimFailedError on a terminal Ready=False reason", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        status: {
          conditions: [
            {
              type: "Ready",
              status: "False",
              reason: "VolumeClaimTemplatesError",
              message: "bad PVC template",
            },
          ],
        },
      });

      const client = new SandboxClient();
      await expect(
        client.createSandbox("warmpool-bad-pvc"),
      ).rejects.toBeInstanceOf(SandboxClaimFailedError);
      expect(mockWatchFn).not.toHaveBeenCalled();
    });

    it("watch path: throws SandboxClaimFailedError on a terminal Ready=False reason", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                {
                  type: "Ready",
                  status: "False",
                  reason: "ClaimExpired",
                  message: "claim TTL elapsed",
                },
              ],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      await expect(
        client.createSandbox("warmpool-expired"),
      ).rejects.toBeInstanceOf(SandboxClaimFailedError);
    });

    it("does NOT throw for a transient Ready=False reason the controller retries", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });

      // First claim watch: transient reason, then the sandbox name.
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            status: {
              conditions: [
                { type: "Ready", status: "False", reason: "SandboxNotReady" },
              ],
              sandbox: { name: "sandbox-transient" },
            },
          });
          return Promise.resolve(new AbortController());
        },
      );
      // Sandbox watch: becomes ready.
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "sandbox-transient", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new SandboxClient();
      const sandbox = await client.createSandbox("warmpool-ok");
      expect(sandbox.sandboxName).toBe("sandbox-transient");
    });
  });

  // ===== getSandbox() closes active handle before evicting from registry =====

  describe("getSandbox() closes active handle on eviction (resource leak fix)", () => {
    it("calls closeLocal() on active handle when claim GET returns 404", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-leak-404");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");
      expect(sandbox1.isActive).toBe(true);

      // Claim GET returns 404 on cache-hit validation
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        SandboxNotFoundError,
      );

      // Active handle must be closed to release kubectl process / tracing span
      expect(sandbox1.isActive).toBe(false);
    });

    it("keeps active handle when claim GET returns non-404 error", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-leak-500");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");
      expect(sandbox1.isActive).toBe(true);

      // Claim GET returns 500 on cache-hit validation
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Internal Server Error"), { code: 500 }),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        SandboxError,
      );

      // The API server failed, not the sandbox: closing the handle here would
      // strand a caller that is still using it.
      expect(sandbox1.isActive).toBe(true);
      expect(client.listActiveSandboxes()).toEqual([
        { namespace: "default", claimName: sandbox1.claimName },
      ]);
    });

    it("calls closeLocal() on active handle when underlying Sandbox CR returns 404", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-leak-cr-404");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");
      expect(sandbox1.isActive).toBe(true);

      // 1st GET: claim verify succeeds with matching sandbox name
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      // 2nd GET: Sandbox CR returns 404
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        SandboxNotFoundError,
      );

      expect(sandbox1.isActive).toBe(false);
    });

    it("calls closeLocal() on active handle when sandboxRef name has changed", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-original-handle");

      const client = new SandboxClient();
      const sandbox1 = await client.createSandbox("tpl");
      expect(sandbox1.isActive).toBe(true);

      // Cache-hit validation returns a *different* sandbox name (sandboxRef changed)
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: "sandbox-new-name" } },
      });

      // Set up re-attach flow for the new sandbox
      mockSandboxReadyFlow("sandbox-new-name");

      const sandbox2 = await client.getSandbox(sandbox1.claimName);

      // Old handle must be closed; new handle must be active
      expect(sandbox1.isActive).toBe(false);
      expect(sandbox2.isActive).toBe(true);
      expect(sandbox2.sandboxName).toBe("sandbox-new-name");
    });
  });

  // ===== getSandboxClaimWarmpoolName =====

  describe("getSandboxClaimWarmpoolName()", () => {
    it("returns warmpool name from claim spec", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        spec: { warmPoolRef: { name: "my-warmpool" } },
      });

      const client = new SandboxClient();
      const name = await client.getSandboxClaimWarmpoolName("my-claim");
      expect(name).toBe("my-warmpool");

      const callArgs = mockGetNamespacedCustomObject.mock.calls[0][0];
      expect(callArgs.group).toBe(CLAIM_API_GROUP);
      expect(callArgs.version).toBe(CLAIM_API_VERSION);
      expect(callArgs.plural).toBe(CLAIM_PLURAL_NAME);
      expect(callArgs.name).toBe("my-claim");
    });

    it("uses provided namespace", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        spec: { warmPoolRef: { name: "pool-in-ns" } },
      });

      const client = new SandboxClient();
      await client.getSandboxClaimWarmpoolName("my-claim", "custom-ns");

      const callArgs = mockGetNamespacedCustomObject.mock.calls[0][0];
      expect(callArgs.namespace).toBe("custom-ns");
    });

    it("throws SandboxNotFoundError when claim returns 404", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const client = new SandboxClient();
      await expect(
        client.getSandboxClaimWarmpoolName("missing-claim"),
      ).rejects.toBeInstanceOf(SandboxNotFoundError);
    });

    it("throws SandboxError on non-404 Kubernetes error", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Service Unavailable"), { code: 503 }),
      );

      const client = new SandboxClient();
      const err = await client
        .getSandboxClaimWarmpoolName("my-claim")
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(SandboxError);
      expect(err).not.toBeInstanceOf(SandboxNotFoundError);
    });
  });

  // ===== resolveSandboxName budget exhaustion =====

  describe("waitForSandboxReady() budget exhaustion", () => {
    it("throws SandboxTimeoutError when resolveSandboxName consumes entire budget", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // Initial GET: claim not yet resolved
      mockGetNamespacedCustomObject.mockResolvedValueOnce({ status: {} });
      // Watch: delays longer than the entire sandboxReadyTimeout before resolving
      mockWatchFn.mockImplementationOnce(
        (
          _p: string,
          _q: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          setTimeout(
            () =>
              callback("MODIFIED", {
                status: { sandbox: { name: "sb-late" } },
              }),
            200, // longer than sandboxReadyTimeout of 50 ms
          );
          return Promise.resolve(new AbortController());
        },
      );

      const { SandboxTimeoutError } = await import("../exceptions.js");
      const client = new SandboxClient({
        sandboxReadyTimeout: 0.05, // 50 ms
      });

      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);
  });
});
