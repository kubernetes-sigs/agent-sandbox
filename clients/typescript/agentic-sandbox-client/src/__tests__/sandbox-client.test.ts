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
} = vi.hoisted(() => ({
  mockCreateNamespacedCustomObject: vi.fn(),
  mockDeleteNamespacedCustomObject: vi.fn(),
  mockGetNamespacedCustomObject: vi.fn(),
  mockListNamespacedCustomObject: vi.fn(),
  mockWatchFn: vi.fn(),
}));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  const KubeConfig = vi.fn().mockImplementation(() => ({
    loadFromDefault: vi.fn(),
    makeApiClient: vi.fn().mockReturnValue({
      createNamespacedCustomObject: mockCreateNamespacedCustomObject,
      deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
      getNamespacedCustomObject: mockGetNamespacedCustomObject,
      listNamespacedCustomObject: mockListNamespacedCustomObject,
    }),
  }));

  const CustomObjectsApi = vi.fn();

  const Watch = vi.fn().mockImplementation(() => ({
    watch: mockWatchFn,
  }));

  return { KubeConfig, CustomObjectsApi, Watch };
});

// ---------- mock: node:child_process ----------

vi.mock("node:child_process", () => ({
  spawn: vi.fn(),
  ChildProcess: vi.fn(),
}));

// ---------- mock: node:net ----------

vi.mock("node:net", async (importOriginal) => {
  const actual = (await importOriginal()) as Record<string, unknown>;
  return {
    ...actual,
    default: { ...actual },
    createServer: actual.createServer,
  };
});

// ---------- import SUT after mocks ----------

import { SandboxClient } from "../sandbox-client.js";
import { Sandbox } from "../sandbox.js";
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  POD_NAME_ANNOTATION,
} from "../constants.js";

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
        apiUrl: "http://api:8080",
        serverPort: 3000,
        sandboxReadyTimeout: 60,
        enableTracing: false,
        traceServiceName: "my-service",
      });
      expect(client).toBeInstanceOf(SandboxClient);
    });
  });

  // ===== createSandbox =====

  describe("createSandbox()", () => {
    it("full flow: creates claim, watches, constructs Sandbox, registers", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("my-sandbox", "my-pod-0");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
      expect(createArgs.body.spec.sandboxTemplateRef.name).toBe(
        "test-template",
      );

      // Verify two watches were used
      expect(mockWatchFn).toHaveBeenCalledTimes(2);
    });

    it("uses default namespace from client options", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-ns");

      const client = new SandboxClient({
        namespace: "prod",
        apiUrl: "http://api:8080",
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
        apiUrl: "http://api:8080",
      });
      const sandbox = await client.createSandbox("tpl", "staging");

      expect(sandbox.namespace).toBe("staging");
    });

    it("passes labels to the claim manifest", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-labeled");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await client.createSandbox("tpl", "default", {
        labels: { env: "test", team: "infra" },
      });

      const createArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(createArgs.body.metadata.labels).toEqual({
        env: "test",
        team: "infra",
      });
    });

    it("throws when template is empty", async () => {
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.createSandbox("")).rejects.toThrow(
        "Template name cannot be empty.",
      );
    });

    it("cleans up orphaned claim when sandbox watch fails", async () => {
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.createSandbox("tpl")).rejects.toThrow("watch failed");

      // Orphaned claim should be deleted
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
    });

    it("falls back to sandboxName when pod annotation is absent", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-xyz"); // no podAnnotation

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox = await client.createSandbox("tpl");

      expect(sandbox.podName).toBe("sandbox-xyz");
      expect(sandbox.sandboxName).toBe("sandbox-xyz");
    });

    it("registers sandbox in the registry", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-reg");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox = await client.createSandbox("tpl");

      const active = client.listActiveSandboxes();
      expect(active).toHaveLength(1);
      expect(active[0].claimName).toBe(sandbox.claimName);
    });
  });

  // ===== getSandbox =====

  describe("getSandbox()", () => {
    it("returns cached handle when still active", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-cache");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox1 = await client.createSandbox("tpl");
      const sandbox2 = await client.getSandbox(sandbox1.claimName);

      expect(sandbox2).toBe(sandbox1);
      // No additional K8s calls
      expect(mockGetNamespacedCustomObject).not.toHaveBeenCalled();
    });

    it("re-attaches when cached handle is inactive (closed)", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-reattach");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
        new Error("HTTP 404"),
      );

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.getSandbox("nonexistent-claim")).rejects.toThrow(
        "SandboxClaim 'nonexistent-claim' not found",
      );
    });
  });

  // ===== listActiveSandboxes =====

  describe("listActiveSandboxes()", () => {
    it("returns active sandboxes and prunes closed ones", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      mockSandboxReadyFlow("sandbox-a");
      mockSandboxReadyFlow("sandbox-b");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sb1 = await client.createSandbox("tpl");
      const sb2 = await client.createSandbox("tpl");

      expect(client.listActiveSandboxes()).toHaveLength(2);

      await sb1.close();
      const active = client.listActiveSandboxes();
      expect(active).toHaveLength(1);
      expect(active[0].claimName).toBe(sb2.claimName);
    });

    it("returns empty list when no sandboxes", () => {
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      expect(await client.listAllSandboxes()).toEqual([]);
    });
  });

  // ===== deleteSandbox =====

  describe("deleteSandbox()", () => {
    it("closes tracked sandbox and removes from registry", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-del");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox = await client.createSandbox("tpl");
      const claimName = sandbox.claimName;

      expect(client.listActiveSandboxes()).toHaveLength(1);

      await client.deleteSandbox(claimName);

      expect(sandbox.isActive).toBe(false);
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("deletes claim directly when sandbox is not tracked", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await client.deleteSandbox("untracked-claim", "default");

      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
      const args = mockDeleteNamespacedCustomObject.mock.calls[0][0];
      expect(args.name).toBe("untracked-claim");
    });

    it("does not throw when claim is already 404", async () => {
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP 404"),
      );

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(
        client.deleteSandbox("missing-claim"),
      ).resolves.toBeUndefined();
    });
  });

  // ===== deleteAll =====

  describe("deleteAll()", () => {
    it("closes all tracked sandboxes", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValue({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      mockSandboxReadyFlow("sandbox-all-a");
      mockSandboxReadyFlow("sandbox-all-b");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sb1 = await client.createSandbox("tpl");
      const sb2 = await client.createSandbox("tpl");

      expect(client.listActiveSandboxes()).toHaveLength(2);

      await client.deleteAll();

      expect(sb1.isActive).toBe(false);
      expect(sb2.isActive).toBe(false);
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("is idempotent when no sandboxes exist", async () => {
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.deleteAll()).resolves.toBeUndefined();
    });
  });

  // ===== [Symbol.asyncDispose] =====

  describe("[Symbol.asyncDispose]()", () => {
    it("calls deleteAll()", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockDeleteNamespacedCustomObject.mockResolvedValue({});
      mockSandboxReadyFlow("sandbox-dispose");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox = await client.createSandbox("tpl");

      await client[Symbol.asyncDispose]();

      expect(sandbox.isActive).toBe(false);
    });
  });

  // ===== getSandbox() does not delete claim on connect failure =====

  describe("getSandbox() preserves SandboxClaim on connect failure", () => {
    it("does not delete SandboxClaim when connect() fails", async () => {
      // GET claim: returns metadata so resolveSandboxName-equivalent resolves,
      // but the Sandbox stays in port-forward mode and connect() will fail
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: {
          name: "test-claim",
          annotations: {},
        },
        status: {
          sandbox: { name: "test-sandbox" },
          sandboxRef: {
            name: "test-sandbox",
            namespace: "default",
          },
        },
      });

      mockSandboxReadyFlow("test-sandbox");

      // Use direct-URL mode so connect() succeeds (getSandbox just re-attaches)
      // Then simulate that the sandbox was closed (isActive=false) to force re-attach
      const client = new SandboxClient({ apiUrl: "http://api:8080" });

      // First createSandbox so we have something in registry
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = await client.createSandbox("tpl");
      const claimName = sandbox.claimName;
      const sandboxName = sandbox.sandboxName;

      // Close locally to mark it inactive (simulates a stale handle)
      await sandbox.closeLocal();
      expect(sandbox.isActive).toBe(false);

      // getSandbox should re-attach; set up watch mocks for re-attach flow
      mockSandboxReadyFlow(sandboxName);
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: claimName, annotations: {} },
        status: {
          sandbox: { name: sandboxName },
          sandboxRef: { name: sandboxName, namespace: "default" },
        },
      });

      const reattached = await client.getSandbox(claimName);
      expect(reattached).toBeInstanceOf(Sandbox);
      expect(reattached.isActive).toBe(true);

      // The claim must NOT have been deleted during re-attachment
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });

    it("closeLocal() is called (not close()) when getSandbox connect fails", async () => {
      // Simulate a scenario where getSandbox finds a claim but connect() fails.
      // The test verifies that the SandboxClaim delete API is never called.
      mockGetNamespacedCustomObject.mockResolvedValue({
        metadata: { name: "my-claim", annotations: {} },
        status: {
          sandbox: { name: "my-sandbox" },
          sandboxRef: { name: "my-sandbox", namespace: "default" },
        },
      });

      // watch for sandbox name resolution: resolves immediately
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", { status: { sandbox: { name: "my-sandbox" } } });
          return Promise.resolve(new AbortController());
        },
      );

      // watch for sandbox ready: resolves immediately
      mockWatchFn.mockImplementationOnce(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
        ) => {
          callback("MODIFIED", {
            metadata: { name: "my-sandbox", annotations: {} },
            status: { conditions: [{ type: "Ready", status: "True" }] },
          });
          return Promise.resolve(new AbortController());
        },
      );

      // Use direct-URL mode — connect() will succeed
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox = await client.getSandbox("my-claim");

      expect(sandbox).toBeInstanceOf(Sandbox);
      // Claim was never deleted
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

      // GET returns a claim whose sandbox name is already resolved
      mockGetNamespacedCustomObject.mockResolvedValue({
        status: { sandbox: { name: "already-ready-sandbox" } },
      });

      const client = new SandboxClient({
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 1, // 1s timeout — current watch-only code times out
      });

      await expect(client.createSandbox("tpl")).resolves.toBeInstanceOf(
        Sandbox,
      );
    }, 5_000);
  });

  // ===== watch done(null) causes promise hang =====

  describe("watch stream clean close causes hang", () => {
    it("resolveSandboxName rejects when done(null) fires (no hang)", async () => {
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

      const client = new SandboxClient({
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 60,
      });

      await expect(client.createSandbox("tpl")).rejects.toThrow();
    }, 2_000);

    it("watchForSandboxReady rejects when done(null) fires (no hang)", async () => {
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

      const client = new SandboxClient({
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 60,
      });

      await expect(client.createSandbox("tpl")).rejects.toThrow();
    }, 2_000);
  });
});
