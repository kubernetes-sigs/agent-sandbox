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
  mockCreateNamespacedCustomObject,
  mockDeleteNamespacedCustomObject,
} = vi.hoisted(() => ({
  mockCreateNamespacedCustomObject: vi.fn(),
  mockDeleteNamespacedCustomObject: vi.fn(),
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
    watch: vi.fn(),
  }));

  return { KubeConfig, CustomObjectsApi, Watch };
});

// ---------- mock: node:child_process ----------

vi.mock("node:child_process", () => ({
  spawn: vi.fn(),
  ChildProcess: vi.fn(),
}));

// ---------- import SUT ----------

import { ComputerUseSandbox } from "../extensions/computer-use.js";

/**
 * Test helper: subclass that exposes protected members.
 */
class TestableComputerUseSandbox extends ComputerUseSandbox {
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

  get _serverPort(): number {
    return this.serverPort;
  }
}

// ---------- tests ----------

describe("ComputerUseSandbox", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // ===== constructor =====

  describe("constructor", () => {
    it("defaults serverPort to 8080", () => {
      const client = new TestableComputerUseSandbox({
        templateName: "computer-use-tpl",
      });
      expect(client._serverPort).toBe(8080);
    });

    it("uses the explicitly provided value", () => {
      const client = new TestableComputerUseSandbox({
        templateName: "computer-use-tpl",
        serverPort: 9090,
      });
      expect(client._serverPort).toBe(9090);
    });
  });

  // ===== agent =====

  describe("agent", () => {
    it("correctly sends queries and parses results", async () => {
      const client = new TestableComputerUseSandbox({
        templateName: "computer-use-tpl",
      });
      client._baseUrl = "http://localhost:7777";
      client._claimName = "cu-claim";

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

      const result = await client.agent("open the browser and search for cats");

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
      expect(opts.headers["X-Sandbox-Port"]).toBe("8080");
    });

    it("throws an error when sandbox is not connected", async () => {
      const client = new TestableComputerUseSandbox({
        templateName: "computer-use-tpl",
      });

      await expect(
        client.agent("do something"),
      ).rejects.toThrow("Sandbox is not ready");
    });
  });
});
