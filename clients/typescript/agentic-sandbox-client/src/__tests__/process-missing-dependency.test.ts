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
//
// Exercises ProcessClient's "optional dependency not installed" path without
// actually uninstalling @bufbuild/protobuf from this repo's node_modules
// (which other tests and the interop e2e suite depend on): vi.mock() throws
// a module-not-found-shaped error from the dynamic import site instead, which
// is what a files-only consumer's real "module not found" would look like.
// This does not substitute for PKG-2's full isolated-consumer install check,
// which needs a separate npm environment to run.

import { afterEach, describe, expect, it, vi } from "vitest";
import type { SandboxError } from "../exceptions.js";

vi.mock("@bufbuild/protobuf", () => {
  const err = new Error("Cannot find package '@bufbuild/protobuf'");
  (err as unknown as { code: string }).code = "ERR_MODULE_NOT_FOUND";
  throw err;
});

afterEach(() => {
  vi.resetModules();
});

describe("ProcessClient missing optional dependency", () => {
  it("reports a fixed, actionable SandboxError instead of a raw module error", async () => {
    const { ProcessClient } = await import("../process.js");
    const client = new ProcessClient({
      grpcBaseUrl: "http://127.0.0.1:1",
      maxCommandOutputSize: 1024,
    });
    await expect(
      client.run(
        { command: ["echo", "hi"] },
        1000,
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({
      telemetryCode: "missing_dependency",
    } satisfies Partial<SandboxError>);
    await expect(
      client.run(
        { command: ["echo", "hi"] },
        1000,
        new AbortController().signal,
      ),
    ).rejects.toThrow(/npm install @bufbuild\/protobuf/);
  });
});
