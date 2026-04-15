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
import type { Mock } from "vitest";

// ---------- hoisted mock fns ----------

const {
  mockDeleteNamespacedCustomObject,
  mockGetNamespacedCustomObject,
  mockWatchFn,
  mockSpawn,
  mockCreateServer,
  mockCreateConnection,
} = vi.hoisted(() => ({
  mockDeleteNamespacedCustomObject: vi.fn(),
  mockGetNamespacedCustomObject: vi.fn(),
  mockWatchFn: vi.fn(),
  mockSpawn: vi.fn(),
  mockCreateServer: vi.fn(),
  mockCreateConnection: vi.fn(),
}));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  const KubeConfig = vi.fn().mockImplementation(() => ({
    loadFromDefault: vi.fn(),
    makeApiClient: vi.fn().mockReturnValue({
      deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
      getNamespacedCustomObject: mockGetNamespacedCustomObject,
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
  spawn: mockSpawn,
  ChildProcess: vi.fn(),
}));

// ---------- mock: node:net ----------

vi.mock("node:net", async (importOriginal) => {
  const actual = (await importOriginal()) as Record<string, unknown>;
  return {
    ...actual,
    default: {
      ...actual,
      createServer: mockCreateServer,
      createConnection: mockCreateConnection,
    },
    createServer: mockCreateServer,
    createConnection: mockCreateConnection,
  };
});

// ---------- import SUT after mocks ----------

import { Sandbox } from "../sandbox.js";
import type { SandboxInit } from "../sandbox.js";
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
} from "../constants.js";

/**
 * Test helper: exposes protected members for test assertions.
 */
class TestableSandbox extends Sandbox {
  get _baseUrl(): string | undefined {
    return this.baseUrl;
  }
  set _baseUrl(value: string | undefined) {
    this.baseUrl = value;
  }

  get _portForwardProcess() {
    return this.portForwardProcess;
  }
  set _portForwardProcess(value) {
    this.portForwardProcess = value;
  }

  get _serverPort(): number {
    return this.serverPort;
  }
}

// ---------- helpers ----------

function makeMockK8sClients() {
  return {
    deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
    getNamespacedCustomObject: mockGetNamespacedCustomObject,
  };
}

function makeMockKubeConfig() {
  return {
    loadFromDefault: vi.fn(),
    makeApiClient: vi.fn().mockReturnValue(makeMockK8sClients()),
  } as unknown as import("@kubernetes/client-node").KubeConfig;
}

function createTestInit(overrides: Partial<SandboxInit> = {}): SandboxInit {
  return {
    claimName: "test-claim",
    sandboxName: "test-sandbox",
    podName: "test-pod",
    namespace: "default",
    annotations: {},
    serverPort: 8888,
    apiUrl: "http://localhost:9999",
    kubeConfig: makeMockKubeConfig(),
    customObjectsApi: makeMockK8sClients() as never,
    traceServiceName: "sandbox-client",
    tracer: null,
    tracingManager: null,
    ...overrides,
  };
}

function createReadySandbox(
  overrides: Partial<SandboxInit> = {},
): TestableSandbox {
  const sandbox = new TestableSandbox(createTestInit(overrides));
  sandbox._baseUrl = "http://localhost:9999";
  return sandbox;
}

/** Returns a fake socket that always emits an ECONNREFUSED error. */
function makeFakeSocketAlwaysError() {
  return {
    destroy: vi.fn(),
    on: vi.fn((event: string, handler: (...args: unknown[]) => void) => {
      if (event === "error") {
        process.nextTick(() =>
          handler(
            Object.assign(new Error("connect ECONNREFUSED"), {
              code: "ECONNREFUSED",
            }),
          ),
        );
      }
    }),
  };
}

// ---------- tests ----------

describe("Sandbox", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // ===== isActive =====

  describe("isActive", () => {
    it("returns true after construction", () => {
      const sandbox = new TestableSandbox(createTestInit());
      expect(sandbox.isActive).toBe(true);
    });

    it("returns false after close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = createReadySandbox();
      expect(sandbox.isActive).toBe(true);
      await sandbox.close();
      expect(sandbox.isActive).toBe(false);
    });
  });

  // ===== commands / files getters =====

  describe("commands / files after close()", () => {
    it("commands throws after close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = createReadySandbox();
      await sandbox.close();
      expect(() => sandbox.commands).toThrow(
        "Sandbox connection has been closed.",
      );
    });

    it("files throws after close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = createReadySandbox();
      await sandbox.close();
      expect(() => sandbox.files).toThrow(
        "Sandbox connection has been closed.",
      );
    });
  });

  // ===== connect() =====

  describe("connect()", () => {
    it("direct mode: sets baseUrl from apiUrl", async () => {
      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: "http://direct-api:8080" }),
      );
      await sandbox.connect();
      expect(sandbox._baseUrl).toBe("http://direct-api:8080");
    });

    it("gateway mode: fetches gateway IP", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        body: { status: { addresses: [{ value: "10.0.0.42" }] } },
      });

      const sandbox = new TestableSandbox(
        createTestInit({
          apiUrl: undefined,
          gatewayName: "kind-gateway",
          gatewayNamespace: "default",
        }),
      );
      await sandbox.connect();
      expect(sandbox._baseUrl).toBe("http://10.0.0.42");
    });

    it("tunnel mode: starts port-forward and sets baseUrl", async () => {
      const fakeServer = {
        listen: vi.fn((_port: number, _host: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 12345 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      const fakeProcess = {
        exitCode: null as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn(),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(fakeProcess);

      // Force crash to abort tunnel (we just want to verify spawn was called)
      fakeProcess.exitCode = 1;

      const sandbox = new TestableSandbox(
        createTestInit({
          apiUrl: undefined,
          portForwardReadyTimeout: 1,
        }),
      );

      await sandbox.connect().catch(() => {});
      expect(mockSpawn).toHaveBeenCalledWith(
        "kubectl",
        expect.arrayContaining(["port-forward"]),
        expect.any(Object),
      );
    });
  });

  // ===== close() =====

  describe("close()", () => {
    it("kills port-forward then deletes the SandboxClaim", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

      const fakeProcess = {
        kill: vi.fn(),
        on: vi.fn((_event: string, cb: () => void) => {
          if (_event === "exit") setTimeout(cb, 0);
        }),
        exitCode: null,
        signalCode: null,
      };

      const sandbox = createReadySandbox();
      sandbox._portForwardProcess = fakeProcess as never;

      await sandbox.close();

      expect(fakeProcess.kill).toHaveBeenCalledWith("SIGTERM");
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
      const args = mockDeleteNamespacedCustomObject.mock.calls[0][0];
      expect(args.group).toBe(CLAIM_API_GROUP);
      expect(args.version).toBe(CLAIM_API_VERSION);
      expect(args.plural).toBe(CLAIM_PLURAL_NAME);
      expect(args.name).toBe("test-claim");
    });

    it("does not throw when claim is 404", async () => {
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP response code was 404"),
      );

      const sandbox = createReadySandbox();
      await expect(sandbox.close()).resolves.toBeUndefined();
    });
  });

  // ===== commands.run =====

  describe("commands.run", () => {
    it("parses command execution results", async () => {
      const sandbox = createReadySandbox();

      (fetch as Mock).mockResolvedValueOnce(
        new Response(
          JSON.stringify({ stdout: "hello\n", stderr: "", exit_code: 0 }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

      const result = await sandbox.commands.run("echo hello");

      expect(result).toEqual({ stdout: "hello\n", stderr: "", exitCode: 0 });

      const [url, opts] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/execute");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body as string)).toEqual({
        command: "echo hello",
      });
      expect(opts.headers["X-Sandbox-ID"]).toBe("test-sandbox");
      expect(opts.headers["X-Sandbox-Namespace"]).toBe("default");
      expect(opts.headers["X-Sandbox-Port"]).toBe("8888");
    });

    it("throws when sandbox is not connected", async () => {
      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined }),
      );
      // baseUrl not set (connect() not called)
      await expect(sandbox.commands.run("ls")).rejects.toThrow(
        "Sandbox is not ready for communication.",
      );
    });
  });

  // ===== files.write =====

  describe("files.write", () => {
    it("uploads string content", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response("ok", { status: 200 }),
      );

      await sandbox.files.write("test.txt", "file content");

      const [url, opts] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/upload");
      expect(opts.method).toBe("POST");
      const file = (opts.body as FormData).get("file") as File;
      expect(file.name).toBe("test.txt");
      expect(await file.text()).toBe("file content");
    });

    it("uploads Buffer content", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response("ok", { status: 200 }),
      );

      const buf = Buffer.from("binary data");
      await sandbox.files.write("output.bin", buf);

      const [, opts] = (fetch as Mock).mock.calls[0];
      const file = (opts.body as FormData).get("file") as File;
      expect(file.name).toBe("output.bin");
      expect(Buffer.from(await file.arrayBuffer()).toString()).toBe(
        "binary data",
      );
    });

    it.each([
      ["sub/foo.txt"],
      ["./foo.txt"],
      ["../foo.txt"],
      ["/abs/foo.txt"],
      ["."],
      [".."],
      ["/"],
    ])("rejects non-plain filename: %s", async (filePath) => {
      const sandbox = createReadySandbox();
      await expect(sandbox.files.write(filePath, "data")).rejects.toThrow(
        /is not a plain filename/,
      );
      expect(fetch).not.toHaveBeenCalled();
    });
  });

  // ===== files.read =====

  describe("files.read", () => {
    it("returns downloaded content as Buffer", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response("downloaded content", { status: 200 }),
      );

      const result = await sandbox.files.read("tmp/hello.txt");

      expect(Buffer.isBuffer(result)).toBe(true);
      expect(result.toString()).toBe("downloaded content");

      const [url] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/download/tmp%2Fhello.txt");
    });
  });

  // ===== files.list =====

  describe("files.list", () => {
    it("returns parsed FileEntry array", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response(
          JSON.stringify([
            { name: "file.txt", size: 100, type: "file", mod_time: 1700000000 },
            {
              name: "subdir",
              size: 4096,
              type: "directory",
              mod_time: 1700000001,
            },
          ]),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

      const entries = await sandbox.files.list("tmp");

      expect(entries).toEqual([
        { name: "file.txt", size: 100, type: "file", modTime: 1700000000 },
        { name: "subdir", size: 4096, type: "directory", modTime: 1700000001 },
      ]);
    });

    it("returns empty array for null response", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify(null), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );

      expect(await sandbox.files.list("empty-dir")).toEqual([]);
    });
  });

  // ===== files.exists =====

  describe("files.exists", () => {
    it("returns true when file exists", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify({ exists: true }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );

      expect(await sandbox.files.exists("tmp/file.txt")).toBe(true);
      expect((fetch as Mock).mock.calls[0][0]).toBe(
        "http://localhost:9999/exists/tmp%2Ffile.txt",
      );
    });

    it("returns false when file does not exist", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify({ exists: false }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );

      expect(await sandbox.files.exists("tmp/missing.txt")).toBe(false);
    });
  });

  // ===== POST /execute is not retried =====

  describe("POST /execute is not retried", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("fetch called once only on 500 — not retried", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValue(
        new Response("server error", { status: 500 }),
      );

      const runPromise = sandbox.commands.run("echo test");
      // Attach rejection handler before advancing timers to prevent
      // Node.js from flagging the rejection as unhandled
      const settled = runPromise.catch(() => {});

      // Flush all retry delays (MAX_RETRIES=5, backoff starts at 500ms)
      await vi.advanceTimersByTimeAsync(60_000);
      await settled;

      expect(fetch).toHaveBeenCalledTimes(1);
    });
  });

  // ===== port-forward timeout deletes claim =====

  describe("port-forward timeout deletes claim", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("SandboxClaim is NOT deleted when port-forward times out", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValue({});

      // getFreePort: synchronous mock server
      const fakeServer = {
        listen: vi.fn((_p: number, _h: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 12345 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      // kubectl: living process (exitCode stays null throughout)
      const fakeProc = {
        exitCode: null as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn(),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(fakeProc);

      // createConnection: always refuses connection
      mockCreateConnection.mockImplementation(() =>
        makeFakeSocketAlwaysError(),
      );

      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined, portForwardReadyTimeout: 0.1 }),
      );

      const connectPromise = sandbox.connect();
      // Attach rejection handler before advancing timers to prevent
      // Node.js from flagging the rejection as unhandled
      const settled = connectPromise.catch(() => {});

      // Advance past the 100ms timeout and all 500ms retry sleeps
      await vi.advanceTimersByTimeAsync(5_000);
      await settled;

      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });
  });
});
