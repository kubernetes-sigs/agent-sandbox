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
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
} from "vitest";
import { SandboxClient } from "agentic-sandbox-client";
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

const WARMPOOL_NAME = "ts-sdk-warmpool";
const COLDPOOL_NAME = "ts-sdk-coldpool";

function getImageTag(): string {
  return process.env["IMAGE_TAG"] ?? "latest";
}

function getImagePrefix(): string {
  return process.env["IMAGE_PREFIX"] ?? "kind.local/";
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
 * Connectivity to the sandbox runtime is a follow-up (see issue #977).
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

// Runtime connectivity (running commands, reading/writing files) lands with the
// sandboxd protocol layer as a follow-up under issue #977. These placeholders
// mark the coverage that suite will add.
describe.skip("TypeScript SDK E2E — sandbox runtime operations (#977)", () => {
  test.todo("runs a command inside the sandbox");
  test.todo("writes and reads a file inside the sandbox");
  test.todo("lists files inside the sandbox");
});
