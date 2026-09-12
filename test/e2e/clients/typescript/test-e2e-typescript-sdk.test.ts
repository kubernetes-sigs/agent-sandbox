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

import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { SandboxClient } from "agentic-sandbox-client";
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
      const ok = await sandbox.commands.run("echo hello from e2e");
      expect(ok.exitCode).toBe(0);
      expect(ok.stdout).toBe("hello from e2e\n");

      const nonZero = await sandbox.commands.run("exit 3");
      expect(nonZero.exitCode).toBe(3);

      const missingCommand = await sandbox.commands.run(
        "definitely-not-a-real-command-e2e",
      );
      expect(missingCommand.exitCode).not.toBe(0);
      expect(missingCommand.exitCode).not.toBe(3);
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
      const catResult = await sandbox.commands.run("cat via-rest.txt");
      expect(catResult.exitCode).toBe(0);
      expect(catResult.stdout).toBe("written via REST\n");

      // Written via the gRPC process API, read back via the REST files API.
      const echoResult = await sandbox.commands.run(
        "printf 'written via gRPC' > via-grpc.txt",
      );
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

      const result = await reattached.commands.run("echo reattached ok");
      expect(result.exitCode).toBe(0);
      expect(result.stdout).toBe("reattached ok\n");
    } finally {
      await reattached.close();
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
      const result = await sandbox.commands.run("echo cold start ok");
      expect(result.exitCode).toBe(0);
      expect(result.stdout).toBe("cold start ok\n");
    } finally {
      await sandbox.close();
    }
  });
});
