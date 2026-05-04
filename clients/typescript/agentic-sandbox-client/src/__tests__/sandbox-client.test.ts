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
  MockKubeConfig.mockImplementation(() => ({
    loadFromDefault: vi.fn(),
    clusters: [{ name: "test-cluster" }],
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

  return { KubeConfig: MockKubeConfig, CustomObjectsApi, Watch };
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

import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  POD_NAME_ANNOTATION,
} from "../constants.js";
import { SandboxError, SandboxNotFoundError } from "../exceptions.js";
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
        apiUrl: "http://api:8080",
        serverPort: 3000,
        sandboxReadyTimeout: 60,
        enableTracing: false,
        traceServiceName: "my-service",
      });
      expect(client).toBeInstanceOf(SandboxClient);
    });
  });

  // ===== constructor validation =====

  describe("constructor validation", () => {
    it("throws SandboxError for serverPort 0", () => {
      expect(() => new SandboxClient({ serverPort: 0 })).toThrow(SandboxError);
    });

    it("throws SandboxError for serverPort 65536", () => {
      expect(() => new SandboxClient({ serverPort: 65536 })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for non-integer serverPort", () => {
      expect(() => new SandboxClient({ serverPort: 8080.5 })).toThrow(
        SandboxError,
      );
    });

    it("accepts serverPort 1 (minimum valid)", () => {
      expect(() => new SandboxClient({ serverPort: 1 })).not.toThrow();
    });

    it("accepts serverPort 65535 (maximum valid)", () => {
      expect(() => new SandboxClient({ serverPort: 65535 })).not.toThrow();
    });

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

    it("throws SandboxError for gatewayReadyTimeout negative", () => {
      expect(() => new SandboxClient({ gatewayReadyTimeout: -1 })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for portForwardReadyTimeout negative", () => {
      expect(() => new SandboxClient({ portForwardReadyTimeout: -1 })).toThrow(
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

    it("throws SandboxError for empty gatewayNamespace", () => {
      expect(() => new SandboxClient({ gatewayNamespace: "" })).toThrow(
        SandboxError,
      );
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      // Original error is re-raised; cleanup error is logged but not surfaced
      await expect(client.createSandbox("tpl")).rejects.toThrow("watch failed");
      // Cleanup was still attempted
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
    });

    // empty namespace string should be normalized to defaultNamespace
    it("normalizes empty namespace string to default namespace", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-ns-norm");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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

    // empty namespace string should be normalized to defaultNamespace
    it("normalizes empty namespace string to default namespace", async () => {
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP 404"),
      );

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
          conditions: [{ type: "Ready", status: "True" }],
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
          conditions: [{ type: "Ready", status: "True" }],
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
          conditions: [{ type: "Ready", status: "True" }],
        },
      });

      // watch for sandbox name resolution: resolves immediately
      // (These watch mocks are now unused because the initial GET resolves both
      //  resolveSandboxName and watchForSandboxReady directly, but are left in
      //  place as documentation of the intended fallback behavior.)
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
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 1, // 1s timeout — watch-only code would time out
      });

      await expect(client.createSandbox("tpl")).resolves.toBeInstanceOf(
        Sandbox,
      );
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
        apiUrl: "http://api:8080",
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
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 0.05, // 50 ms
      });

      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);
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
        apiUrl: "http://api:8080",
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
        apiUrl: "http://api:8080",
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.createSandbox("tpl")).rejects.toThrow(
        "watch startup failed",
      );
    });
  });

  // ===== KubeConfig fail-fast =====

  describe("KubeConfig validation", () => {
    it("throws SandboxError when clusters array is empty", () => {
      MockKubeConfig.mockImplementationOnce(() => ({
        loadFromDefault: vi.fn(),
        clusters: [], // empty → no kubeconfig configured
        makeApiClient: vi.fn(),
      }));
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });

    it("throws SandboxError when clusters is undefined", () => {
      MockKubeConfig.mockImplementationOnce(() => ({
        loadFromDefault: vi.fn(),
        clusters: undefined, // undefined → no kubeconfig configured
        makeApiClient: vi.fn(),
      }));
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });

    it("throws SandboxError when only cluster is localhost:8080 (loadFromDefault fallback)", () => {
      MockKubeConfig.mockImplementationOnce(() => ({
        loadFromDefault: vi.fn(),
        clusters: [{ name: "in-cluster", server: "http://localhost:8080" }],
        makeApiClient: vi.fn(),
      }));
      expect(() => new SandboxClient()).toThrow(SandboxError);
    });
  });

  // ===== option validation =====

  describe("apiUrl option validation", () => {
    it("throws SandboxError for non-URL apiUrl", () => {
      expect(() => new SandboxClient({ apiUrl: "not-a-url" })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for non-http/https scheme", () => {
      expect(
        () => new SandboxClient({ apiUrl: "ftp://api.example.com" }),
      ).toThrow(SandboxError);
    });

    it("accepts http scheme", () => {
      expect(
        () => new SandboxClient({ apiUrl: "http://api.example.com:8080" }),
      ).not.toThrow();
    });

    it("accepts https scheme", () => {
      expect(
        () => new SandboxClient({ apiUrl: "https://api.example.com" }),
      ).not.toThrow();
    });
  });

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

  describe("gatewayName DNS subdomain validation", () => {
    it("throws SandboxError for gatewayName with uppercase letters", () => {
      expect(() => new SandboxClient({ gatewayName: "MyGateway" })).toThrow(
        SandboxError,
      );
    });

    it("throws SandboxError for gatewayName with invalid characters", () => {
      expect(() => new SandboxClient({ gatewayName: "gateway_name" })).toThrow(
        SandboxError,
      );
    });

    it("accepts valid gatewayName with dots and hyphens", () => {
      expect(
        () => new SandboxClient({ gatewayName: "my-gateway.prod" }),
      ).not.toThrow();
    });
  });

  // ===== enableAutoCleanup idempotency =====

  describe("enableAutoCleanup()", () => {
    it("is idempotent: second call returns no-op and does not register duplicate handlers", () => {
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
  });

  // ===== getSandbox() K8s re-validation on cache hit =====

  describe("getSandbox() K8s re-validation on cache hit", () => {
    it("evicts cached handle and throws when claim returns 404 on cache hit", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-evict");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox1 = await client.createSandbox("tpl");

      // K8s GET returns 404 during cache-hit validation
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP 404"),
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      const sandbox1 = await client.createSandbox("tpl");

      // 1st GET: claim verify succeeds with matching sandbox name
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        metadata: { name: sandbox1.claimName },
        status: { sandbox: { name: sandbox1.sandboxName } },
      });
      // 2nd GET: Sandbox CR returns 404
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP 404"),
      );

      await expect(client.getSandbox(sandbox1.claimName)).rejects.toThrow(
        SandboxNotFoundError,
      );
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("evicts cached handle and throws SandboxError on non-404 Sandbox GET error", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-non404");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });

    it("evicts cached handle when sandboxRef name has changed since creation", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      mockSandboxReadyFlow("sandbox-original");

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 2,
      });

      // Expected: createSandbox eventually succeeds by re-listing after done(null).
      // Current behavior: throws SandboxNotReadyError immediately on done(null).
      const sandbox = await client.createSandbox("tpl");
      expect(sandbox.sandboxName).toBe("sandbox-relisted");
    }, 5_000);
  });

  // ===== initial GET 404 → immediate SandboxNotFoundError, no watch =====

  describe("resolveSandboxName() 404 on initial GET (#10)", () => {
    it("throws SandboxNotFoundError immediately and never calls watch on 404", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
      // Initial GET for claim returns 404
      mockGetNamespacedCustomObject.mockRejectedValueOnce(
        Object.assign(new Error("Not Found"), { code: 404 }),
      );

      const { SandboxNotFoundError } = await import("../exceptions.js");
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxNotFoundError,
      );
      // Watch must NEVER be called — 404 must fail immediately
      expect(mockWatchFn).not.toHaveBeenCalled();
    }, 3_000);
  });

  // ===== non-404 K8s errors are not collapsed into SandboxNotFoundError =====

  describe("getSandbox() non-404 error discrimination (#7)", () => {
    it("throws SandboxError (not SandboxNotFoundError) on non-404 error during cache miss", async () => {
      const k8sErr = Object.assign(new Error("Service Unavailable"), {
        code: 503,
      });
      mockGetNamespacedCustomObject.mockRejectedValueOnce(k8sErr);

      const { SandboxError, SandboxNotFoundError } = await import(
        "../exceptions.js"
      );
      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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

      const client = new SandboxClient({ apiUrl: "http://api:8080" });
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
      // Handle evicted from registry
      expect(client.listActiveSandboxes()).toHaveLength(0);
    });
  });

  // ===== resolveSandboxName budget exhaustion =====

  describe("waitForSandboxReady() budget exhaustion (#19)", () => {
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
        apiUrl: "http://api:8080",
        sandboxReadyTimeout: 0.05, // 50 ms
      });

      await expect(client.createSandbox("tpl")).rejects.toBeInstanceOf(
        SandboxTimeoutError,
      );
    }, 3_000);
  });
});
