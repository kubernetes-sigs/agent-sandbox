/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import * as crypto from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { SandboxClient, SandboxdRpcError } from "agentic-sandbox-client";
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
} from "vitest";
import { TestContext } from "./framework/context.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const TEST_MANIFESTS_DIR = path.join(__dirname, "test-manifests");
const TEMPLATE_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-template.yaml",
);
const WARMPOOL_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-warmpool.yaml",
);
const COLDPOOL_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-coldpool.yaml",
);
const SANDBOXD_TEMPLATE_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-sandboxd-template.yaml",
);
const SANDBOXD_WARMPOOL_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-sandboxd-warmpool.yaml",
);
const SANDBOXD_COLDPOOL_YAML_PATH = path.join(
  TEST_MANIFESTS_DIR,
  "sandbox-sandboxd-coldpool.yaml",
);

const WARMPOOL_NAME = "ts-sdk-warmpool";
const COLDPOOL_NAME = "ts-sdk-coldpool";
const SANDBOXD_WARMPOOL_NAME = "ts-sdk-sandboxd-warmpool";
const SANDBOXD_COLDPOOL_NAME = "ts-sdk-sandboxd-coldpool";

function getImageTag(): string {
  return process.env.IMAGE_TAG ?? "latest";
}

function getImagePrefix(): string {
  return process.env.IMAGE_PREFIX ?? "kind.local/";
}

/**
 * Deploys the SandboxTemplate into the test namespace.
 */
function deploySandboxTemplate(tc: TestContext, namespace: string): void {
  const manifest = fs
    .readFileSync(TEMPLATE_YAML_PATH, "utf-8")
    .replaceAll("{image_prefix}", getImagePrefix())
    .replaceAll("{image_tag}", getImageTag());
  tc.applyManifestText(manifest, namespace);
}

/**
 * Deploys the warm pool and waits for it to be ready.
 */
async function deployWarmPool(
  tc: TestContext,
  namespace: string,
): Promise<void> {
  const manifest = fs.readFileSync(WARMPOOL_YAML_PATH, "utf-8");
  tc.applyManifestText(manifest, namespace);
  await tc.waitForWarmPoolReady(WARMPOOL_NAME, namespace);
}

/**
 * Deploys the cold pool (replicas: 0) for tests that provision sandboxes
 * without pre-warmed slots.
 */
function deployColdPool(tc: TestContext, namespace: string): void {
  const manifest = fs.readFileSync(COLDPOOL_YAML_PATH, "utf-8");
  tc.applyManifestText(manifest, namespace);
}

/**
 * Deploys the sandboxd Template/WarmPool pair (runtimeClassName unspecified —
 * the connect layer is runtime-independent, but the required E2E coverage
 * here is limited to the standard kind default runtime) and waits for the
 * warm pool to be ready.
 */
async function deploySandboxdWarmPool(
  tc: TestContext,
  namespace: string,
): Promise<void> {
  const templateManifest = fs
    .readFileSync(SANDBOXD_TEMPLATE_YAML_PATH, "utf-8")
    .replaceAll("{image_prefix}", getImagePrefix())
    .replaceAll("{image_tag}", getImageTag());
  tc.applyManifestText(templateManifest, namespace);

  const warmPoolManifest = fs.readFileSync(
    SANDBOXD_WARMPOOL_YAML_PATH,
    "utf-8",
  );
  tc.applyManifestText(warmPoolManifest, namespace);
  await tc.waitForWarmPoolReady(SANDBOXD_WARMPOOL_NAME, namespace);
}

/**
 * Deploys the sandboxd Template plus a replicas:0 cold pool, for the cold
 * (no pre-warmed slot) provisioning path.
 */
function deploySandboxdColdPool(tc: TestContext, namespace: string): void {
  const templateManifest = fs
    .readFileSync(SANDBOXD_TEMPLATE_YAML_PATH, "utf-8")
    .replaceAll("{image_prefix}", getImagePrefix())
    .replaceAll("{image_tag}", getImageTag());
  tc.applyManifestText(templateManifest, namespace);

  const coldPoolManifest = fs.readFileSync(
    SANDBOXD_COLDPOOL_YAML_PATH,
    "utf-8",
  );
  tc.applyManifestText(coldPoolManifest, namespace);
}

/**
 * Polls listAllSandboxes() until the claim no longer appears.
 */
async function waitForClaimDeleted(
  client: SandboxClient,
  claimName: string,
  namespace: string,
  timeoutMs = 60_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const claims = await client.listAllSandboxes(namespace);
    if (!claims.includes(claimName)) return;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(
    `SandboxClaim '${claimName}' was not deleted within ${timeoutMs}ms`,
  );
}

/**
 * Splits `data` into fixed-size chunks delivered one at a time via pull(),
 * so a writeStream() test actually exercises multi-chunk streaming instead
 * of handing fetch a single already-complete blob.
 */
function toReadableStream(
  data: Uint8Array,
  chunkSize = 64 * 1024,
): ReadableStream<Uint8Array> {
  let offset = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (offset >= data.byteLength) {
        controller.close();
        return;
      }
      const end = Math.min(offset + chunkSize, data.byteLength);
      controller.enqueue(data.subarray(offset, end));
      offset = end;
    },
  });
}

/** Reads a ReadableStream to completion and concatenates it into one buffer. */
async function readAll(
  stream: ReadableStream<Uint8Array>,
): Promise<Uint8Array> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    total += value.byteLength;
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

function sha256Hex(data: Uint8Array): string {
  return crypto.createHash("sha256").update(data).digest("hex");
}

/**
 * Exercises the Kubernetes resource layer: provision a Sandbox from a pool,
 * assert its identity and that the SandboxClaim exists, then delete it.
 * Runtime connectivity (running commands, reading/writing files) is covered
 * separately below, against the sandboxd-backed warm pool.
 */
async function runResourceLayerChecks(
  client: SandboxClient,
  poolName: string,
  namespace: string,
): Promise<void> {
  const sandbox = await client.createSandbox(poolName, namespace);

  expect(sandbox.claimName).toMatch(/^sandbox-claim-/);
  expect(sandbox.sandboxName).toBeTruthy();
  expect(sandbox.podName).toBeTruthy();
  expect(sandbox.namespace).toBe(namespace);
  expect(sandbox.isActive).toBe(true);

  const claims = await client.listAllSandboxes(namespace);
  expect(claims).toContain(sandbox.claimName);

  // getSandbox() re-resolves the same identity from the live claim.
  const fetched = await client.getSandbox(sandbox.claimName, namespace);
  expect(fetched.sandboxName).toBe(sandbox.sandboxName);

  await sandbox.close();
  expect(sandbox.isActive).toBe(false);

  await waitForClaimDeleted(client, sandbox.claimName, namespace);
}

describe("TypeScript SDK E2E — Kubernetes resource layer", () => {
  let tc: TestContext;
  let namespace: string;

  beforeAll(() => {
    tc = new TestContext();
  });

  beforeEach(async () => {
    namespace = await tc.createTempNamespace("ts-sdk-e2e-");
  });

  afterEach(async () => {
    await tc.deleteNamespace(namespace);
  });

  test("provisions and deletes a sandbox from a cold pool", async () => {
    deploySandboxTemplate(tc, namespace);
    deployColdPool(tc, namespace);

    const client = new SandboxClient({ namespace });
    await runResourceLayerChecks(client, COLDPOOL_NAME, namespace);
  });

  test("provisions and deletes a sandbox from a warm pool", async () => {
    deploySandboxTemplate(tc, namespace);
    await deployWarmPool(tc, namespace);

    const client = new SandboxClient({ namespace });
    await runResourceLayerChecks(client, WARMPOOL_NAME, namespace);
  });
});

describe("TypeScript SDK E2E — sandbox runtime operations (sandboxd)", () => {
  let tc: TestContext;
  let namespace: string;

  beforeAll(() => {
    tc = new TestContext();
  });

  beforeEach(async () => {
    namespace = await tc.createTempNamespace("ts-sdk-sandboxd-e2e-");
    await deploySandboxdWarmPool(tc, namespace);
  });

  afterEach(async () => {
    await tc.deleteNamespace(namespace);
  });

  test("runs commands, and distinguishes a non-zero exit from a missing command", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      // Each argv element reaches the process verbatim — no shell word
      // splitting or expansion.
      const ok = await sandbox.commands.run("echo", ["hello from", "$HOME"]);
      expect(ok.exitCode).toBe(0);
      expect(ok.stdout).toBe("hello from $HOME\n");

      const nonZero = await sandbox.commands.run("sh", ["-c", "exit 3"]);
      expect(nonZero.exitCode).toBe(3);

      // With no shell in between, a missing executable is sandboxd's
      // NOT_FOUND, not a shell's exit 127.
      await expect(
        sandbox.commands.run("definitely-not-a-real-command-e2e"),
      ).rejects.toSatisfy(
        (err: unknown) =>
          err instanceof SandboxdRpcError && err.code === "not_found",
      );
    } finally {
      await sandbox.close();
    }
  });

  test("runs commands with env and cwd", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      const withEnv = await sandbox.commands.run(
        "sh",
        ["-c", 'printf %s "$E2E_GREETING"'],
        { env: { E2E_GREETING: "hello env" } },
      );
      expect(withEnv.exitCode).toBe(0);
      expect(withEnv.stdout).toBe("hello env");

      await sandbox.files.write("cwd-dir/in-cwd.txt", "found via cwd\n");
      const withCwd = await sandbox.commands.run("cat", ["in-cwd.txt"], {
        cwd: "cwd-dir",
      });
      expect(withCwd.exitCode).toBe(0);
      expect(withCwd.stdout).toBe("found via cwd\n");

      await expect(
        sandbox.commands.run("pwd", { cwd: "../.." }),
      ).rejects.toSatisfy(
        (err: unknown) =>
          err instanceof SandboxdRpcError && err.code === "permission_denied",
      );
    } finally {
      await sandbox.close();
    }
  });

  test("writes, reads, lists, checks existence of, and deletes files", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      await sandbox.files.write("greeting.txt", "hello from the TS SDK e2e\n");

      const content = await sandbox.files.read("greeting.txt");
      expect(new TextDecoder().decode(content)).toBe(
        "hello from the TS SDK e2e\n",
      );

      await expect(sandbox.files.exists("greeting.txt")).resolves.toBe(true);
      await expect(sandbox.files.exists("does-not-exist.txt")).resolves.toBe(
        false,
      );

      const listing = await sandbox.files.list(".");
      expect(listing.entries.map((e) => e.name)).toContain("greeting.txt");

      await sandbox.files.delete("greeting.txt");
      await expect(sandbox.files.exists("greeting.txt")).resolves.toBe(false);
    } finally {
      await sandbox.close();
    }
  });

  test("files (REST) and commands (gRPC) share the same working directory", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      // Written via the REST files API, read back via the gRPC process API.
      await sandbox.files.write("via-rest.txt", "written via REST\n");
      const catResult = await sandbox.commands.run("cat", ["via-rest.txt"]);
      expect(catResult.exitCode).toBe(0);
      expect(catResult.stdout).toBe("written via REST\n");

      // Written via the gRPC process API, read back via the REST files API.
      const echoResult = await sandbox.commands.run("sh", [
        "-c",
        "printf 'written via gRPC' > via-grpc.txt",
      ]);
      expect(echoResult.exitCode).toBe(0);
      const content = await sandbox.files.read("via-grpc.txt");
      expect(new TextDecoder().decode(content)).toBe("written via gRPC");
    } finally {
      await sandbox.close();
    }
  });

  test("getSandbox() re-attaches to a live sandbox from a fresh SandboxClient", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    const { claimName, sandboxName, podName } = sandbox;

    await sandbox.files.write("reattach.txt", "written before re-attach\n");

    // closeLocal() drops only this handle's local connection — the
    // SandboxClaim (and the pod behind it) must outlive it, mirroring an
    // agent process restarting while its sandbox keeps running.
    await sandbox.closeLocal();

    const freshClient = new SandboxClient({ namespace });
    const reattached = await freshClient.getSandbox(claimName, namespace);
    try {
      expect(reattached.claimName).toBe(claimName);
      expect(reattached.sandboxName).toBe(sandboxName);
      expect(reattached.podName).toBe(podName);

      const content = await reattached.files.read("reattach.txt");
      expect(new TextDecoder().decode(content)).toBe(
        "written before re-attach\n",
      );

      const result = await reattached.commands.run("echo", ["reattached ok"]);
      expect(result.exitCode).toBe(0);
      expect(result.stdout).toBe("reattached ok\n");
    } finally {
      await reattached.close();
    }
  });

  test("writeStream/readStream round-trip a multi-megabyte payload through the real PodTunnel", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      const data = crypto.randomBytes(4 * 1024 * 1024);
      const expectedHash = sha256Hex(data);

      await sandbox.files.writeStream(
        "stream-roundtrip.bin",
        toReadableStream(data),
        { timeoutMs: 120_000 },
      );

      const stream = await sandbox.files.readStream("stream-roundtrip.bin", {
        timeoutMs: 120_000,
      });
      const readBack = await readAll(stream);
      // Correctness is judged by content hash and the server's own size
      // accounting, never by elapsed time or a fixed sleep.
      expect(sha256Hex(readBack)).toBe(expectedHash);

      const listing = await sandbox.files.list(".");
      const entry = listing.entries.find(
        (e) => e.name === "stream-roundtrip.bin",
      );
      expect(entry?.size).toBe(data.byteLength);
    } finally {
      await sandbox.close();
    }
  });

  test("cancelling readStream after one chunk still leaves the sandbox usable for later calls", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      const data = crypto.randomBytes(2 * 1024 * 1024);
      await sandbox.files.writeStream("cancel-me.bin", toReadableStream(data), {
        timeoutMs: 120_000,
      });

      const stream = await sandbox.files.readStream("cancel-me.bin", {
        timeoutMs: 120_000,
      });
      const reader = stream.getReader();
      await reader.read();
      await reader.cancel();

      // The same connection generation must still serve subsequent
      // operations — both the REST files API and the gRPC process API.
      await sandbox.files.write("after-cancel.txt", "still alive\n");
      const readBack = await sandbox.files.read("after-cancel.txt");
      expect(new TextDecoder().decode(readBack)).toBe("still alive\n");

      const runResult = await sandbox.commands.run("echo", ["after cancel ok"]);
      expect(runResult.exitCode).toBe(0);
      expect(runResult.stdout).toBe("after cancel ok\n");
    } finally {
      await sandbox.close();
    }
  });

  test("writeStream enforces maxUploadSize without creating a new file or corrupting an existing one", async () => {
    const client = new SandboxClient({
      namespace,
      // Well below the SDK default (256 MiB) so the overflow path triggers
      // on a small, fast payload instead of a multi-hundred-MiB one.
      sandboxd: { maxUploadSize: 1024 * 1024 },
    });
    const sandbox = await client.createSandbox(
      SANDBOXD_WARMPOOL_NAME,
      namespace,
    );
    try {
      const oversized = crypto.randomBytes(2 * 1024 * 1024);

      await expect(
        sandbox.files.writeStream(
          "new-oversized.bin",
          toReadableStream(oversized),
        ),
      ).rejects.toMatchObject({ telemetryCode: "request_too_large" });
      await expect(sandbox.files.exists("new-oversized.bin")).resolves.toBe(
        false,
      );

      const original = crypto.randomBytes(4096);
      const originalHash = sha256Hex(original);
      await sandbox.files.write("existing.bin", original);

      await expect(
        sandbox.files.writeStream("existing.bin", toReadableStream(oversized)),
      ).rejects.toMatchObject({ telemetryCode: "request_too_large" });

      const stillThere = await sandbox.files.read("existing.bin");
      expect(sha256Hex(stillThere)).toBe(originalHash);
    } finally {
      await sandbox.close();
    }
  });
});

describe("TypeScript SDK E2E — sandbox runtime operations (sandboxd, cold pool)", () => {
  let tc: TestContext;
  let namespace: string;

  beforeAll(() => {
    tc = new TestContext();
  });

  beforeEach(async () => {
    namespace = await tc.createTempNamespace("ts-sdk-sandboxd-cold-e2e-");
    deploySandboxdColdPool(tc, namespace);
  });

  afterEach(async () => {
    await tc.deleteNamespace(namespace);
  });

  test("runs a command against a sandbox provisioned without a pre-warmed slot", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_COLDPOOL_NAME,
      namespace,
    );
    try {
      const result = await sandbox.commands.run("echo", ["cold start ok"]);
      expect(result.exitCode).toBe(0);
      expect(result.stdout).toBe("cold start ok\n");
    } finally {
      await sandbox.close();
    }
  });

  test("writeStream/readStream round-trip a multi-megabyte payload against a cold-provisioned sandbox", async () => {
    const client = new SandboxClient({ namespace });
    const sandbox = await client.createSandbox(
      SANDBOXD_COLDPOOL_NAME,
      namespace,
    );
    try {
      // Smaller than the warm-pool round-trip test: this test's point is
      // that the streaming path works through cold connection
      // initialization too, not to re-verify large-payload throughput.
      const data = crypto.randomBytes(2 * 1024 * 1024);
      const expectedHash = sha256Hex(data);

      await sandbox.files.writeStream(
        "cold-stream-roundtrip.bin",
        toReadableStream(data),
        { timeoutMs: 120_000 },
      );

      const stream = await sandbox.files.readStream(
        "cold-stream-roundtrip.bin",
        { timeoutMs: 120_000 },
      );
      const readBack = await readAll(stream);
      expect(sha256Hex(readBack)).toBe(expectedHash);

      const listing = await sandbox.files.list(".");
      const entry = listing.entries.find(
        (e) => e.name === "cold-stream-roundtrip.bin",
      );
      expect(entry?.size).toBe(data.byteLength);
    } finally {
      await sandbox.close();
    }
  });
});
