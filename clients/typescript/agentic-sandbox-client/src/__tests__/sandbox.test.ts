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
  MAX_DRAIN_BYTES,
  MAX_UPLOAD_SIZE,
  PER_ATTEMPT_TIMEOUT_MS,
} from "../constants.js";
import {
  SandboxPortForwardError,
  SandboxRequestError,
  SandboxTimeoutError,
} from "../exceptions.js";

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

  // Review #9: expose reconnect flag for test assertions
  get _isReconnecting(): boolean {
    return (this as unknown as { _reconnecting: boolean })._reconnecting;
  }
  set _isReconnecting(value: boolean) {
    (this as unknown as { _reconnecting: boolean })._reconnecting = value;
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

    it("pre-captured commands reference cannot make requests after sandbox.close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = createReadySandbox();

      // Capture reference before close
      const cmd = sandbox.commands;

      await sandbox.close();

      // Expected: cmd.run() throws without calling fetch.
      // Current behavior: baseUrl is still set on the Sandbox, so the pre-captured
      // CommandExecutor's request() call proceeds and fetch is invoked.
      await expect(cmd.run("ls")).rejects.toThrow();
      expect(fetch).not.toHaveBeenCalled();
    });

    it("pre-captured files reference cannot make requests after sandbox.close()", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});
      const sandbox = createReadySandbox();

      // Capture reference before close
      const files = sandbox.files;

      await sandbox.close();

      // Expected: files.exists() throws without calling fetch.
      await expect(files.exists("test.txt")).rejects.toThrow();
      expect(fetch).not.toHaveBeenCalled();
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

    it("gateway mode: wraps IPv6 address in brackets for baseUrl", async () => {
      mockGetNamespacedCustomObject.mockResolvedValueOnce({
        body: { status: { addresses: [{ value: "2001:db8::1" }] } },
      });

      const sandbox = new TestableSandbox(
        createTestInit({
          apiUrl: undefined,
          gatewayName: "kind-gateway",
          gatewayNamespace: "default",
        }),
      );
      await sandbox.connect();
      // Expected: "http://[2001:db8::1]" — IPv6 must be wrapped in brackets.
      // Current implementation inserts the raw value without brackets,
      // producing invalid URL "http://2001:db8::1".
      expect(sandbox._baseUrl).toMatch(/\[2001:db8::1\]/);
    });

    it("tunnel mode: spawn 'error' event throws SandboxPortForwardError without closing sandbox", async () => {
      const fakeServer = {
        listen: vi.fn((_p: number, _h: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 54321 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      const spawnError = new Error("ENOENT: kubectl not found");
      const fakeProc = {
        exitCode: null as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        // Register 'error' event handler — fires on the next tick
        on: vi.fn((event: string, handler: (err?: Error) => void) => {
          if (event === "error") {
            process.nextTick(() => handler(spawnError));
          }
        }),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(fakeProc);
      mockCreateConnection.mockImplementation(() =>
        makeFakeSocketAlwaysError(),
      );

      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined, portForwardReadyTimeout: 0.05 }),
      );

      // Expected: SandboxPortForwardError wrapping the spawn error is thrown
      // quickly (before the 50ms timeout), AND isActive stays true because
      // closeLocal() is not called.
      // Current behavior: 'error' event is not listened to. The polling loop
      // runs until the 50ms timeout, then calls closeLocal() → isActive = false.
      await expect(sandbox.connect()).rejects.toBeInstanceOf(
        SandboxPortForwardError,
      );

      // isActive must remain true — spawn error is a transport error, not a logical close.
      // Current behavior: isActive = false (timeout path called closeLocal()).
      expect(sandbox.isActive).toBe(true);
    });
  });

  // ===== closeLocal() =====

  describe("closeLocal()", () => {
    it("kills port-forward and marks sandbox inactive WITHOUT deleting the SandboxClaim", async () => {
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

      await sandbox.closeLocal();

      expect(fakeProcess.kill).toHaveBeenCalledWith("SIGTERM");
      expect(sandbox.isActive).toBe(false);
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
    });

    it("is idempotent: second call does not kill a null process or call K8s", async () => {
      const sandbox = createReadySandbox();
      await sandbox.closeLocal();
      await sandbox.closeLocal(); // second call should be a no-op
      expect(mockDeleteNamespacedCustomObject).not.toHaveBeenCalled();
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

    it("rejects when content exceeds MAX_UPLOAD_SIZE", async () => {
      const sandbox = createReadySandbox();
      const bigContent = Buffer.alloc(MAX_UPLOAD_SIZE + 1);

      await expect(
        sandbox.files.write("big.bin", bigContent),
      ).rejects.toBeInstanceOf(SandboxRequestError);
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

    it("fully encodes RFC 3986 sub-delimiters in path (!, ', (, ), *)", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response("data", { status: 200 }),
      );

      await sandbox.files.read("a!b'c(d)e*f");

      const [url] = (fetch as Mock).mock.calls[0];
      // encodeURIComponent leaves ! ' ( ) * unencoded; encodePathSegment must encode them
      expect(url).toBe("http://localhost:9999/download/a%21b%27c%28d%29e%2Af");
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

    it("fully encodes RFC 3986 sub-delimiters in exists path", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify({ exists: true }), { status: 200 }),
      );

      await sandbox.files.exists("dir/file!name.txt");

      const [url] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/exists/dir%2Ffile%21name.txt");
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

  // ===== response body drain before retry =====

  describe("response body drain before retry", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("calls response.text() to drain body before retrying on 503", async () => {
      const sandbox = createReadySandbox();

      // Spy on the text() method of the first (503) response
      const drainSpy = vi.fn().mockResolvedValue("service unavailable");
      const errorResponse = Object.assign(
        new Response("service unavailable", { status: 503 }),
        { text: drainSpy },
      );
      const okResponse = new Response(JSON.stringify({ exists: true }), {
        status: 200,
      });
      (fetch as Mock)
        .mockResolvedValueOnce(errorResponse)
        .mockResolvedValueOnce(okResponse);

      const existsPromise = sandbox.files.exists("test.txt");
      const settled = existsPromise.catch(() => {});

      // Flush the backoff delay
      await vi.advanceTimersByTimeAsync(5_000);
      await settled;

      // The drain spy must have been called before the retry
      expect(drainSpy).toHaveBeenCalled();
      expect(fetch).toHaveBeenCalledTimes(2);
    });
  });

  // ===== overall timeout stops retry =====

  describe("overall timeout stops retry loop", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("aborts retry loop when overall request timeout fires", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockRejectedValue(
        Object.assign(new Error("ECONNREFUSED"), { code: "ECONNREFUSED" }),
      );

      // list() uses GET (retryable); timeout = 0.1 s = 100 ms
      const listPromise = sandbox.files.list(".", 0.1);
      const settled = listPromise.catch(() => {});

      // Advance 200 ms — past the 100 ms overall timeout but before the
      // first 500 ms backoff sleep completes
      await vi.advanceTimersByTimeAsync(200);
      await settled;

      // The overall timeout fired during the backoff sleep, stopping the loop
      // after the very first attempt — fetch should be called exactly once
      expect(fetch).toHaveBeenCalledTimes(1);
    });
  });

  // ===== SandboxRequestError includes body and operation =====

  describe("SandboxRequestError includes HTTP body and operation", () => {
    it("sets body and operation fields on non-2xx response", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockResolvedValueOnce(
        new Response('{"error":"not found"}', { status: 404 }),
      );

      const err = await sandbox.commands.run("cat missing").catch((e) => e);

      expect(err.constructor.name).toBe("SandboxRequestError");
      expect(err.statusCode).toBe(404);
      expect(err.body).toBe('{"error":"not found"}');
      expect(err.operation).toBe("POST execute");
    });

    it("truncates body to MAX_ERROR_BODY_BYTES (512)", async () => {
      const sandbox = createReadySandbox();
      const longBody = "x".repeat(1000);
      (fetch as Mock).mockResolvedValueOnce(
        new Response(longBody, { status: 500 }),
      );

      const err = await sandbox.commands.run("big output").catch((e) => e);

      expect(err.body).toHaveLength(512);
    });
  });

  // ===== port-forward auto-reconnect =====

  describe("port-forward auto-reconnect", () => {
    it("reconnects port-forward when process has died before a request", async () => {
      // Fake a free-port server
      const fakeServer = {
        listen: vi.fn((_p: number, _h: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 54321 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      // Fake a kubectl process that starts alive
      const fakeProc = {
        exitCode: null as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn((ev: string, cb: () => void) => {
          if (ev === "exit") setTimeout(cb, 0);
        }),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(fakeProc);

      // createConnection: succeed immediately for the reconnect port-forward check
      mockCreateConnection.mockImplementation(
        (_opts: unknown, cb: () => void) => {
          const sock = {
            destroy: vi.fn(),
            on: vi.fn(),
          };
          process.nextTick(cb);
          return sock;
        },
      );

      // Create sandbox in port-forward mode, manually set a dead process + valid baseUrl
      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined, portForwardReadyTimeout: 1 }),
      );
      sandbox._baseUrl = "http://127.0.0.1:12345";

      // Simulate port-forward process death
      const deadProc = {
        exitCode: 1,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn((ev: string, cb: () => void) => {
          if (ev === "exit") setTimeout(cb, 0);
        }),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      sandbox._portForwardProcess = deadProc as never;

      // fetch returns OK after reconnect
      (fetch as Mock).mockResolvedValueOnce(
        new Response(JSON.stringify({ exists: true }), { status: 200 }),
      );

      await sandbox.files.exists("test.txt");

      // SIGTERM was sent to the dead process
      expect(deadProc.kill).toHaveBeenCalledWith("SIGTERM");
      // A new kubectl process was spawned for the reconnect
      expect(mockSpawn).toHaveBeenCalled();
      // fetch was eventually called (reconnect succeeded)
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

  describe("port-forward reconnect failure does not permanently close sandbox", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("sandbox.isActive remains true after reconnect timeout (transport error ≠ logical close)", async () => {
      const fakeServer = {
        listen: vi.fn((_p: number, _h: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 54321 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      // Reconnect attempts: process stays ALIVE (exitCode = null) but TCP never connects.
      // This forces the timeout path in startAndWaitForPortForward(), which currently
      // calls closeLocal() and sets _isClosed = true.
      const failingProc = {
        exitCode: null as number | null, // ALIVE — no crash detection
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn(),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(failingProc);
      mockCreateConnection.mockImplementation(() =>
        makeFakeSocketAlwaysError(),
      );

      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined, portForwardReadyTimeout: 0.05 }),
      );
      sandbox._baseUrl = "http://127.0.0.1:12345";

      const deadProc = {
        exitCode: 1 as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn((ev: string, cb: () => void) => {
          if (ev === "exit") setTimeout(cb, 0);
        }),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      sandbox._portForwardProcess = deadProc as never;

      const requestPromise = sandbox.files.exists("test.txt");
      const settled = requestPromise.catch(() => {});

      // Advance past the reconnect timeout and all backoff delays
      await vi.advanceTimersByTimeAsync(30_000);
      await settled;

      // Expected: isActive remains true — reconnect failure is a transport error,
      // not a logical close. The caller can retry later.
      // Current behavior: startAndWaitForPortForward timeout calls closeLocal(),
      // setting _isClosed = true and making isActive return false.
      expect(sandbox.isActive).toBe(true);
    });
  });

  describe("response body drain is bounded by MAX_DRAIN_BYTES", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("does not call response.text() (unbounded read) when draining a 503 body", async () => {
      const sandbox = createReadySandbox();

      // 503 response with body larger than MAX_DRAIN_BYTES
      const largeBody = "x".repeat(MAX_DRAIN_BYTES + 1024);
      const errorResponse = new Response(largeBody, { status: 503 });

      // Spy on text() — if called, that means unbounded drain is happening
      const textSpy = vi
        .spyOn(errorResponse, "text")
        .mockResolvedValue(largeBody);

      const okResponse = new Response(JSON.stringify({ exists: true }), {
        status: 200,
      });
      (fetch as Mock)
        .mockResolvedValueOnce(errorResponse)
        .mockResolvedValueOnce(okResponse);

      const existsPromise = sandbox.files.exists("test.txt");
      const settled = existsPromise.catch(() => {});

      await vi.advanceTimersByTimeAsync(10_000);
      await settled;

      // Expected: text() is NOT called (bounded reader is used instead).
      // Current behavior: fetchWithRetry calls response.text() for unbounded drain.
      expect(textSpy).not.toHaveBeenCalled();
    });
  });

  describe("overall timeout throws SandboxTimeoutError", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("aborts with SandboxTimeoutError, not DOMException, when overall timeout fires", async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockRejectedValue(
        Object.assign(new Error("ECONNREFUSED"), { code: "ECONNREFUSED" }),
      );

      // timeout = 0.1 s
      const listPromise = sandbox.files.list(".", 0.1);
      const errPromise = listPromise.catch((e) => e);

      await vi.advanceTimersByTimeAsync(200);
      const caught = await errPromise;

      // Expected: SandboxTimeoutError (distinct from transport errors).
      // Current behavior: DOMException("signal timed out", "TimeoutError") or
      // SandboxRequestError — neither is SandboxTimeoutError.
      expect(caught).toBeInstanceOf(SandboxTimeoutError);
    });
  });

  describe("concurrent requests wait for reconnect completion", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("second request should wait for reconnect to finish, not fail after fixed 2s", async () => {
      const sandbox = new TestableSandbox(
        createTestInit({ apiUrl: undefined, portForwardReadyTimeout: 5 }),
      );

      // Dead process — request() will call reconnect()
      const deadProc = {
        exitCode: 1 as number | null,
        signalCode: null as string | null,
        kill: vi.fn(),
        on: vi.fn((ev: string, cb: () => void) => {
          if (ev === "exit") setTimeout(cb, 0);
        }),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      sandbox._portForwardProcess = deadProc as never;

      // Mark as already reconnecting (simulates 1st concurrent request owns the reconnect)
      sandbox._isReconnecting = true;
      // baseUrl not yet restored by the ongoing reconnect

      (fetch as Mock).mockResolvedValue(
        new Response(JSON.stringify({ exists: true }), { status: 200 }),
      );

      // Second request — hits the `if (_reconnecting)` branch → fixed 2s sleep
      const requestPromise = sandbox.files.exists("test.txt");
      const errPromise = requestPromise.catch((e) => e);

      // Advance 2s (the fixed sleep fires) but baseUrl is still not set yet
      await vi.advanceTimersByTimeAsync(2000);
      // Simulate reconnect completing at t=2500ms (after the 2s window)
      sandbox._baseUrl = "http://127.0.0.1:54321";
      await vi.advanceTimersByTimeAsync(600);

      const result = await errPromise;

      // Expected (after Phase 3): second request shares the reconnect promise and
      // succeeds when reconnect finishes at t=2500ms, so result === true.
      // Current behavior: request() checks baseUrl at t=0 (before any reconnect wait),
      // finds it undefined, and throws SandboxNotReadyError immediately.
      expect(result).toBe(true);
    });
  });

  describe("close() does not hang beyond cleanup timeout", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("completes (or throws) within timeout even if K8s delete never resolves", async () => {
      // deleteNamespacedCustomObject never resolves — simulates a hung API server
      mockDeleteNamespacedCustomObject.mockImplementation(
        () => new Promise<void>(() => {}),
      );

      const sandbox = createReadySandbox();
      const closePromise = sandbox.close();
      const settled = closePromise
        .then(() => "resolved")
        .catch(() => "rejected");

      // Advance 10 seconds — well beyond any reasonable cleanup timeout
      await vi.advanceTimersByTimeAsync(10_000);

      // Expected: close() completes (resolves or rejects) within cleanup budget.
      // Current behavior: close() hangs indefinitely because deleteNamespacedCustomObject
      // never resolves and there is no cleanup timeout.
      const result = await Promise.race([settled, Promise.resolve("hanging")]);
      expect(result).not.toBe("hanging");
    });
  });

  describe("per-attempt timeout stops at response headers, not body read", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    // Requires real fetch to observe: AbortSignal.timeout wired to body stream is not
    // reproducible with mock fetch — skip until Phase 3 adds an integration-level check.
    it.skip("large response body read succeeds even if body takes longer than PER_ATTEMPT_TIMEOUT_MS", async () => {
      const sandbox = createReadySandbox();

      // Body arrives PER_ATTEMPT_TIMEOUT_MS + 5000 ms after headers.
      // The response stream respects the fetch AbortSignal so that the current
      // per-attempt abort-all-of-fetch behavior is observable.
      const bodyDelayMs = PER_ATTEMPT_TIMEOUT_MS + 5_000;

      (fetch as Mock).mockImplementation(
        async (_url: string, opts?: RequestInit) => {
          const signal = opts?.signal as AbortSignal | undefined;
          const stream = new ReadableStream({
            start(controller) {
              const timer = setTimeout(() => {
                controller.enqueue(
                  new TextEncoder().encode(JSON.stringify({ exists: true })),
                );
                controller.close();
              }, bodyDelayMs);

              // Connect the AbortSignal to the stream so we can observe abort behavior
              signal?.addEventListener("abort", () => {
                clearTimeout(timer);
                controller.error(
                  signal.reason ?? new DOMException("aborted", "AbortError"),
                );
              });
            },
          });
          return new Response(stream, { status: 200 });
        },
      );

      const existsPromise = sandbox.files.exists("test.txt");
      const errPromise = existsPromise.catch((e) => e);

      // Advance past PER_ATTEMPT_TIMEOUT_MS to fire the per-attempt signal
      await vi.advanceTimersByTimeAsync(PER_ATTEMPT_TIMEOUT_MS + 1_000);
      // Advance remaining delay so body would arrive (only relevant if signal didn't abort)
      await vi.advanceTimersByTimeAsync(5_000);

      const result = await errPromise;

      // Expected: result is true — per-attempt timeout stops at headers, not body.
      // Current behavior: the per-attempt AbortSignal is still active during body read.
      // When it fires, the stream aborts → result is an AbortError or SandboxRequestError.
      expect(result).toBe(true);
    });
  });

  describe("empty path is rejected by files methods", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it('files.list("") rejects immediately without making a request', async () => {
      const sandbox = createReadySandbox();
      // Reject immediately to prevent an infinite real-timer hang in the retry loop
      (fetch as Mock).mockRejectedValue(
        new Error("unexpected: fetch was called"),
      );

      const listPromise = sandbox.files.list("");
      const settled = listPromise.catch(() => {});
      // Advance past all retry backoffs so the promise settles
      await vi.advanceTimersByTimeAsync(60_000);
      await settled;

      // Expected: validation fires before fetch is called.
      // Current behavior: empty path is forwarded to GET /list/, so fetch IS called.
      expect(fetch).not.toHaveBeenCalled();
    });

    it('files.exists("") rejects immediately without making a request', async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockRejectedValue(
        new Error("unexpected: fetch was called"),
      );

      const existsPromise = sandbox.files.exists("");
      const settled = existsPromise.catch(() => {});
      await vi.advanceTimersByTimeAsync(60_000);
      await settled;

      expect(fetch).not.toHaveBeenCalled();
    });

    it('files.read("") rejects immediately without making a request', async () => {
      const sandbox = createReadySandbox();
      (fetch as Mock).mockRejectedValue(
        new Error("unexpected: fetch was called"),
      );

      const readPromise = sandbox.files.read("");
      const settled = readPromise.catch(() => {});
      await vi.advanceTimersByTimeAsync(60_000);
      await settled;

      expect(fetch).not.toHaveBeenCalled();
    });
  });
});
