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
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// ---------- hoisted mock fns ----------

const {
  mockCreateNamespacedCustomObject,
  mockDeleteNamespacedCustomObject,
  mockWatchFn,
} = vi.hoisted(() => ({
  mockCreateNamespacedCustomObject: vi.fn(),
  mockDeleteNamespacedCustomObject: vi.fn(),
  mockWatchFn: vi.fn(),
}));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  const KubeConfig = vi.fn().mockImplementation(() => ({
    loadFromDefault: vi.fn(),
    clusters: [{ name: "test-cluster" }],
    makeApiClient: vi.fn().mockReturnValue({
      createNamespacedCustomObject: mockCreateNamespacedCustomObject,
      deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
      listNamespacedCustomObject: vi.fn().mockResolvedValue({ items: [] }),
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

// ---------- import SUT ----------

import { POD_NAME_ANNOTATION } from "../constants.js";
import { SandboxRequestError } from "../exceptions.js";
import {
  ComputerUseSandbox,
  ComputerUseSandboxClient,
} from "../extensions/computer-use.js";
import type { SandboxInit } from "../sandbox.js";
import { Sandbox } from "../sandbox.js";

/**
 * Test helper: exposes protected members for test assertions.
 */
class TestableComputerUseSandbox extends ComputerUseSandbox {
  get _baseUrl(): string | undefined {
    return this.baseUrl;
  }
  set _baseUrl(value: string | undefined) {
    this.baseUrl = value;
  }

  get _serverPort(): number {
    return this.serverPort;
  }
}

function makeBaseInit(): SandboxInit {
  return {
    claimName: "cu-claim",
    sandboxName: "cu-sandbox",
    podName: "cu-pod",
    namespace: "default",
    annotations: {},
    serverPort: 8888,
    apiUrl: "http://localhost:7777",
    kubeConfig: {} as never,
    customObjectsApi: {
      deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
    } as never,
    traceServiceName: "sandbox-client",
    tracer: null,
    tracingManager: null,
  };
}

// ---------- tests ----------

describe("ComputerUseSandbox (handle)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // ===== constructor =====

  describe("constructor", () => {
    it("preserves serverPort 8888 (no override)", () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      expect(sandbox._serverPort).toBe(8888);
    });

    it("preserves explicitly set serverPort", () => {
      const sandbox = new TestableComputerUseSandbox({
        ...makeBaseInit(),
        serverPort: 9090,
      });
      expect(sandbox._serverPort).toBe(9090);
    });

    it("is an instance of Sandbox", () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      expect(sandbox).toBeInstanceOf(Sandbox);
    });
  });

  // ===== agent() =====

  describe("agent()", () => {
    it("sends query and parses result", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";

      (fetch as Mock).mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            stdout: "task completed",
            stderr: "",
            exit_code: 0,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

      const result = await sandbox.agent(
        "open the browser and search for cats",
      );

      expect(result).toEqual({
        stdout: "task completed",
        stderr: "",
        exitCode: 0,
      });

      expect(fetch).toHaveBeenCalledOnce();
      const [url, opts] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:7777/agent");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body as string)).toEqual({
        query: "open the browser and search for cats",
      });
      expect(opts.headers["X-Sandbox-Port"]).toBe("8888");
    });

    it("throws when sandbox is not connected (no baseUrl)", async () => {
      const sandbox = new TestableComputerUseSandbox({
        ...makeBaseInit(),
        apiUrl: undefined,
      });
      // Do not call connect(), so baseUrl is undefined

      await expect(sandbox.agent("do something")).rejects.toThrow(
        "Sandbox is not ready",
      );
    });
  });

  // ===== POST /agent is not retried =====

  describe("POST /agent is not retried", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("fetch called once only on 500 — not retried", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";
      (fetch as Mock).mockResolvedValue(
        new Response("server error", { status: 500 }),
      );

      const agentPromise = sandbox.agent("do something");
      // Attach rejection handler before advancing timers to prevent
      // Node.js from flagging the rejection as unhandled
      const settled = agentPromise.catch(() => {});

      // Flush all retry delays
      await vi.advanceTimersByTimeAsync(60_000);
      await settled;

      expect(fetch).toHaveBeenCalledTimes(1);
    });
  });

  // ===== agent() error handling =====

  describe("agent() error handling", () => {
    it("throws SandboxRequestError with SyntaxError cause on malformed JSON", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";
      (fetch as Mock).mockResolvedValueOnce(
        new Response("not valid json {{", { status: 200 }),
      );

      const err = await sandbox.agent("do something").catch((e) => e);

      expect(err).toBeInstanceOf(SandboxRequestError);
      expect(err.cause).toBeInstanceOf(SyntaxError);
    });

    it("throws SandboxRequestError when JSON top-level is null", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify(null), { status: 200 }),
      );

      const err = await sandbox.agent("do something").catch((e) => e);

      expect(err).toBeInstanceOf(SandboxRequestError);
      expect(err.message).toMatch(/expected object/i);
    });

    it("throws SandboxRequestError when JSON top-level is a string", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify("oops"), { status: 200 }),
      );

      const err = await sandbox.agent("do something").catch((e) => e);

      expect(err).toBeInstanceOf(SandboxRequestError);
      expect(err.message).toMatch(/expected object/i);
    });

    it("returns ExecutionResult without throwing on non-zero exit_code and stderr", async () => {
      const sandbox = new TestableComputerUseSandbox(makeBaseInit());
      sandbox._baseUrl = "http://localhost:7777";
      (fetch as Mock).mockResolvedValueOnce(
        new Response(
          JSON.stringify({ stdout: "", stderr: "boom", exit_code: 1 }),
          { status: 200 },
        ),
      );

      const result = await sandbox.agent("do something");

      expect(result).toEqual({ stdout: "", stderr: "boom", exitCode: 1 });
    });
  });
});

// ---------- ComputerUseSandboxClient (registry) ----------

describe("ComputerUseSandboxClient (registry)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function mockSandboxReadyFlow(sandboxName: string): void {
    mockWatchFn.mockImplementationOnce(
      (
        _path: string,
        _query: unknown,
        callback: (type: string, obj: Record<string, unknown>) => void,
      ) => {
        callback("MODIFIED", { status: { sandbox: { name: sandboxName } } });
        return Promise.resolve(new AbortController());
      },
    );

    mockWatchFn.mockImplementationOnce(
      (
        _path: string,
        _query: unknown,
        callback: (type: string, obj: Record<string, unknown>) => void,
      ) => {
        callback("MODIFIED", {
          metadata: {
            name: sandboxName,
            annotations: { [POD_NAME_ANNOTATION]: `${sandboxName}-pod` },
          },
          status: { conditions: [{ type: "Ready", status: "True" }] },
        });
        return Promise.resolve(new AbortController());
      },
    );
  }

  it("createSandbox() returns a ComputerUseSandbox handle", async () => {
    mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
    mockSandboxReadyFlow("cu-sandbox-1");

    const client = new ComputerUseSandboxClient({ apiUrl: "http://api:8080" });
    const sandbox = await client.createSandbox("cu-template");

    expect(sandbox).toBeInstanceOf(ComputerUseSandbox);
    expect(sandbox).toBeInstanceOf(Sandbox);
    expect(sandbox.isActive).toBe(true);
  });

  it("created sandbox has serverPort 8888 by default", async () => {
    mockCreateNamespacedCustomObject.mockResolvedValueOnce({});
    mockSandboxReadyFlow("cu-sandbox-2");

    const client = new ComputerUseSandboxClient({ apiUrl: "http://api:8080" });
    const sandbox = (await client.createSandbox(
      "cu-template",
    )) as TestableComputerUseSandbox;

    // ComputerUseSandbox uses serverPort 8888 (no override)
    // Since TestableComputerUseSandbox is not used here, verify via agent() header
    (fetch as Mock).mockResolvedValueOnce(
      new Response(JSON.stringify({ stdout: "", stderr: "", exit_code: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await sandbox.agent("test query");
    expect((fetch as Mock).mock.calls[0][1].headers["X-Sandbox-Port"]).toBe(
      "8888",
    );
  });
});
