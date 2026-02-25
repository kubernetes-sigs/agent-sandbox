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

// ---------- hoisted mock fns (accessible inside vi.mock factories) ----------

const {
  mockCreateNamespacedCustomObject,
  mockDeleteNamespacedCustomObject,
  mockWatchFn,
  mockSpawn,
  mockCreateServer,
} = vi.hoisted(() => ({
  mockCreateNamespacedCustomObject: vi.fn(),
  mockDeleteNamespacedCustomObject: vi.fn(),
  mockWatchFn: vi.fn(),
  mockSpawn: vi.fn(),
  mockCreateServer: vi.fn(),
}));

// ---------- mock: @kubernetes/client-node ----------

vi.mock("@kubernetes/client-node", () => {
  const KubeConfig = vi.fn().mockImplementation(() => ({
    loadFromDefault: vi.fn(),
    makeApiClient: vi.fn().mockReturnValue({
      createNamespacedCustomObject: mockCreateNamespacedCustomObject,
      deleteNamespacedCustomObject: mockDeleteNamespacedCustomObject,
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
    default: { ...actual, createServer: mockCreateServer },
    createServer: mockCreateServer,
  };
});

// ---------- import SUT after mocks ----------

import { SandboxClient } from "../sandbox-client.js";
import {
  CLAIM_API_GROUP,
  CLAIM_API_VERSION,
  CLAIM_PLURAL_NAME,
  POD_NAME_ANNOTATION,
} from "../constants.js";

/**
 * Test helper: subclass that exposes protected members.
 */
class TestableSandboxClient extends SandboxClient {
  get _baseUrl(): string | undefined {
    return this.baseUrl;
  }
  set _baseUrl(value: string | undefined) {
    this.baseUrl = value;
  }

  get _claimName(): string | undefined {
    return this.claimName;
  }
  set _claimName(value: string | undefined) {
    this.claimName = value;
  }

  get _sandboxName(): string | undefined {
    return this.sandboxName;
  }
  set _sandboxName(value: string | undefined) {
    this.sandboxName = value;
  }

  get _podName(): string | undefined {
    return this.podName;
  }
  set _podName(value: string | undefined) {
    this.podName = value;
  }

  get _namespace(): string {
    return this.namespace;
  }

  get _serverPort(): number {
    return this.serverPort;
  }

  get _templateName(): string {
    return this.templateName;
  }

  get _portForwardProcess() {
    return this.portForwardProcess;
  }
  set _portForwardProcess(value) {
    this.portForwardProcess = value;
  }

  get _customObjectsApi() {
    return this.customObjectsApi;
  }
}

// ---------- helpers ----------

function createReadyClient(overrides: Partial<{
  baseUrl: string;
  claimName: string;
  sandboxName: string;
  namespace: string;
  serverPort: number;
}> = {}): TestableSandboxClient {
  const client = new TestableSandboxClient({
    templateName: "test-template",
    namespace: overrides.namespace ?? "default",
    serverPort: overrides.serverPort,
  });
  client._baseUrl = overrides.baseUrl ?? "http://localhost:9999";
  client._claimName = overrides.claimName ?? "test-claim";
  client._sandboxName = overrides.sandboxName ?? "test-sandbox";
  return client;
}

// ---------- tests ----------

describe("SandboxClient", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // ===== constructor =====

  describe("constructor", () => {
    it("correctly sets default options", () => {
      const client = new TestableSandboxClient({
        templateName: "my-template",
      });

      expect(client._templateName).toBe("my-template");
      expect(client._namespace).toBe("default");
      expect(client._serverPort).toBe(8888);
      expect(client.isReady()).toBe(false);
    });

    it("reflects custom options", () => {
      const client = new TestableSandboxClient({
        templateName: "custom-tpl",
        namespace: "prod",
        serverPort: 3000,
        apiUrl: "http://my-api:8080",
      });

      expect(client._templateName).toBe("custom-tpl");
      expect(client._namespace).toBe("prod");
      expect(client._serverPort).toBe(3000);
      expect(client._baseUrl).toBe("http://my-api:8080");
    });
  });

  // ===== isReady =====

  describe("isReady", () => {
    it("returns false when baseUrl is not set", () => {
      const client = new TestableSandboxClient({
        templateName: "tpl",
      });
      expect(client.isReady()).toBe(false);
    });

    it("returns true when apiUrl is set", () => {
      const client = new TestableSandboxClient({
        templateName: "tpl",
        apiUrl: "http://localhost:8080",
      });
      expect(client.isReady()).toBe(true);
    });
  });

  // ===== run =====

  describe("run", () => {
    it("correctly parses command execution results", async () => {
      const client = createReadyClient();

      (fetch as Mock).mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            stdout: "hello world\n",
            stderr: "",
            exit_code: 0,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

      const result = await client.run("echo hello world");

      expect(result).toEqual({
        stdout: "hello world\n",
        stderr: "",
        exitCode: 0,
      });

      expect(fetch).toHaveBeenCalledOnce();
      const [url, opts] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/execute");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(opts.body as string)).toEqual({
        command: "echo hello world",
      });
      expect(opts.headers["X-Sandbox-ID"]).toBe("test-claim");
      expect(opts.headers["X-Sandbox-Namespace"]).toBe("default");
      expect(opts.headers["X-Sandbox-Port"]).toBe("8888");
    });

    it("throws an error when sandbox is not connected", async () => {
      const client = new TestableSandboxClient({
        templateName: "tpl",
      });

      await expect(client.run("ls")).rejects.toThrow(
        "Sandbox is not ready for communication.",
      );
    });
  });

  // ===== write =====

  describe("write", () => {
    it("successfully uploads string content", async () => {
      const client = createReadyClient();

      (fetch as Mock).mockResolvedValueOnce(
        new Response("ok", { status: 200 }),
      );

      await client.write("/tmp/test.txt", "file content");

      expect(fetch).toHaveBeenCalledOnce();
      const [url, opts] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/upload");
      expect(opts.method).toBe("POST");
      expect(opts.body).toBeInstanceOf(FormData);

      const formData = opts.body as FormData;
      const file = formData.get("file") as File;
      expect(file).toBeTruthy();
      expect(file.name).toBe("test.txt");

      const text = await file.text();
      expect(text).toBe("file content");
    });

    it("successfully uploads Buffer content", async () => {
      const client = createReadyClient();

      (fetch as Mock).mockResolvedValueOnce(
        new Response("ok", { status: 200 }),
      );

      const buf = Buffer.from("binary data");
      await client.write("/data/output.bin", buf);

      expect(fetch).toHaveBeenCalledOnce();
      const [, opts] = (fetch as Mock).mock.calls[0];
      const formData = opts.body as FormData;
      const file = formData.get("file") as File;
      expect(file.name).toBe("output.bin");

      const arrayBuf = await file.arrayBuffer();
      expect(Buffer.from(arrayBuf).toString()).toBe("binary data");
    });
  });

  // ===== read =====

  describe("read", () => {
    it("returns file download result as a Buffer", async () => {
      const client = createReadyClient();
      const content = "downloaded content";

      (fetch as Mock).mockResolvedValueOnce(
        new Response(content, { status: 200 }),
      );

      const result = await client.read("tmp/hello.txt");

      expect(Buffer.isBuffer(result)).toBe(true);
      expect(result.toString()).toBe("downloaded content");

      const [url] = (fetch as Mock).mock.calls[0];
      expect(url).toBe("http://localhost:9999/download/tmp/hello.txt");
    });
  });

  // ===== start / close lifecycle =====

  describe("start / close lifecycle", () => {
    it("start(): executes flow createClaim -> waitForSandboxReady -> port-forward", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      // mockWatch: immediately fire a MODIFIED event with Ready condition
      mockWatchFn.mockImplementation(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
          _done: (err: unknown) => void,
        ) => {
          callback("MODIFIED", {
            metadata: {
              name: "my-sandbox",
              annotations: { [POD_NAME_ANNOTATION]: "my-pod-0" },
            },
            status: {
              conditions: [{ type: "Ready", status: "True" }],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      // mock getFreePort: override net.createServer
      const fakeServer = {
        listen: vi.fn((_port: number, _host: string, cb: () => void) => cb()),
        address: vi.fn(() => ({ port: 12345 })),
        close: vi.fn((cb: () => void) => cb()),
        on: vi.fn(),
      };
      mockCreateServer.mockReturnValue(fakeServer);

      // mock spawn for port-forward
      const fakeProcess = {
        exitCode: null as number | null,
        kill: vi.fn(),
        on: vi.fn(),
        stdout: { on: vi.fn() },
        stderr: { on: vi.fn() },
      };
      mockSpawn.mockReturnValue(fakeProcess);

      const client = new TestableSandboxClient({
        templateName: "tpl",
        portForwardReadyTimeout: 1,
      });

      // Since port-forward requires actual socket connectivity, force exit
      // to make the loop fail fast after verifying createClaim + watch
      fakeProcess.exitCode = 1;

      await client.start().catch(() => {
        // expected: tunnel crash because we faked exitCode
      });

      // Verify createClaim was called
      expect(mockCreateNamespacedCustomObject).toHaveBeenCalledOnce();
      const callArgs = mockCreateNamespacedCustomObject.mock.calls[0][0];
      expect(callArgs.group).toBe(CLAIM_API_GROUP);
      expect(callArgs.version).toBe(CLAIM_API_VERSION);
      expect(callArgs.plural).toBe(CLAIM_PLURAL_NAME);
      expect(callArgs.namespace).toBe("default");

      // Verify Watch was used for sandbox readiness
      expect(mockWatchFn).toHaveBeenCalledOnce();

      // Verify pod name was extracted from annotation
      expect(client._podName).toBe("my-pod-0");
      expect(client._sandboxName).toBe("my-sandbox");
    });

    it("start() with apiUrl: completes flow createClaim -> waitForSandboxReady", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      mockWatchFn.mockImplementation(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
          _done: (err: unknown) => void,
        ) => {
          callback("MODIFIED", {
            metadata: {
              name: "my-sandbox-2",
              annotations: {},
            },
            status: {
              conditions: [{ type: "Ready", status: "True" }],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new TestableSandboxClient({
        templateName: "tpl",
        apiUrl: "http://direct-api:8080",
      });

      await client.start();

      expect(mockCreateNamespacedCustomObject).toHaveBeenCalledOnce();
      expect(mockWatchFn).toHaveBeenCalledOnce();
      expect(client.isReady()).toBe(true);
      expect(client._baseUrl).toBe("http://direct-api:8080");
    });

    it("close(): executes port-forward kill -> delete claim", async () => {
      mockDeleteNamespacedCustomObject.mockResolvedValueOnce({});

      const fakeProcess = {
        kill: vi.fn(),
        on: vi.fn((_event: string, cb: () => void) => {
          if (_event === "exit") {
            setTimeout(cb, 0);
          }
        }),
        exitCode: null,
      };

      const client = createReadyClient();
      client._portForwardProcess = fakeProcess as never;

      await client.close();

      expect(fakeProcess.kill).toHaveBeenCalledWith("SIGTERM");
      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
      const callArgs = mockDeleteNamespacedCustomObject.mock.calls[0][0];
      expect(callArgs.group).toBe(CLAIM_API_GROUP);
      expect(callArgs.version).toBe(CLAIM_API_VERSION);
      expect(callArgs.plural).toBe(CLAIM_PLURAL_NAME);
      expect(callArgs.name).toBe("test-claim");
    });

    it("close(): does not error if claim is 404", async () => {
      mockDeleteNamespacedCustomObject.mockRejectedValueOnce(
        new Error("HTTP response code was 404"),
      );

      const client = createReadyClient();
      // Should not throw
      await client.close();

      expect(mockDeleteNamespacedCustomObject).toHaveBeenCalledOnce();
    });
  });

  // ===== Pod name discovery =====

  describe("Pod name discovery", () => {
    it("uses pod-name from annotation if present", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      mockWatchFn.mockImplementation(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
          _done: (err: unknown) => void,
        ) => {
          callback("ADDED", {
            metadata: {
              name: "sandbox-abc",
              annotations: {
                [POD_NAME_ANNOTATION]: "custom-pod-name",
              },
            },
            status: {
              conditions: [{ type: "Ready", status: "True" }],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new TestableSandboxClient({
        templateName: "tpl",
        apiUrl: "http://api:8080",
      });

      await client.start();
      expect(client._podName).toBe("custom-pod-name");
    });

    it("falls back to sandbox name if annotation is missing", async () => {
      mockCreateNamespacedCustomObject.mockResolvedValueOnce({});

      mockWatchFn.mockImplementation(
        (
          _path: string,
          _query: unknown,
          callback: (type: string, obj: Record<string, unknown>) => void,
          _done: (err: unknown) => void,
        ) => {
          callback("ADDED", {
            metadata: {
              name: "sandbox-xyz",
              annotations: {},
            },
            status: {
              conditions: [{ type: "Ready", status: "True" }],
            },
          });
          return Promise.resolve(new AbortController());
        },
      );

      const client = new TestableSandboxClient({
        templateName: "tpl",
        apiUrl: "http://api:8080",
      });

      await client.start();
      expect(client._podName).toBe("sandbox-xyz");
      expect(client._sandboxName).toBe("sandbox-xyz");
    });
  });
});
