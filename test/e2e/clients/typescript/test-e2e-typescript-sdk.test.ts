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
import { fileURLToPath } from "node:url";

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

// Project root is 4 levels up from test/e2e/clients/typescript/
const PROJECT_ROOT = path.resolve(__dirname, "../../../..");
const ROUTER_YAML_PATH = path.join(
  PROJECT_ROOT,
  "clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml",
);
const GATEWAY_YAML_PATH = path.join(
  PROJECT_ROOT,
  "clients/python/agentic-sandbox-client/gateway-kind/gateway-kind.yaml",
);

const GATEWAY_NAME = "kind-gateway";

const TEMPLATE_NAME = "ts-sdk-test-template";
const WARMPOOL_NAME = "ts-sdk-warmpool";

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
  const imageTag = getImageTag();
  const imagePrefix = getImagePrefix();
  const manifest = fs
    .readFileSync(TEMPLATE_YAML_PATH, "utf-8")
    .replace("{image_prefix}", imagePrefix)
    .replace("{image_tag}", imageTag);
  tc.applyManifestText(manifest, namespace);
}

/**
 * Deploys the sandbox router and waits for it to be ready.
 */
async function deployRouter(tc: TestContext, namespace: string): Promise<void> {
  const imageTag = getImageTag();
  const imagePrefix = getImagePrefix();
  const routerImage = `${imagePrefix}sandbox-router:${imageTag}`;
  console.log(`Using router image: ${routerImage}`);

  const manifest = fs
    .readFileSync(ROUTER_YAML_PATH, "utf-8")
    .replace("IMAGE_PLACEHOLDER", routerImage);

  console.log(`Applying router manifest to namespace: ${namespace}`);
  tc.applyManifestText(manifest, namespace);

  console.log("Waiting for router deployment to be ready...");
  await tc.waitForDeploymentReady("sandbox-router-deployment", namespace);
}

/**
 * Deploys the gateway and waits for an address.
 */
async function deployGateway(
  tc: TestContext,
  namespace: string,
): Promise<void> {
  const manifest = fs.readFileSync(GATEWAY_YAML_PATH, "utf-8");

  console.log(`Applying gateway manifest to namespace: ${namespace}`);
  tc.applyManifestText(manifest, namespace);

  console.log("Waiting for gateway to get an address...");
  await tc.waitForGatewayAddress(GATEWAY_NAME, namespace);
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
  console.log("Warmpool manifest applied.");

  await tc.waitForWarmPoolReady(WARMPOOL_NAME, namespace);
  console.log("Warmpool is ready.");
}

/**
 * Runs basic SDK operations to validate functionality.
 */
async function runSdkTests(sandbox: SandboxClient): Promise<void> {
  // Test execution
  const result = await sandbox.run("echo 'Hello from SDK'");
  console.log(`Run result: ${JSON.stringify(result)}`);
  expect(result.stdout).toBe("Hello from SDK\n");
  expect(result.stderr).toBe("");
  expect(result.exitCode).toBe(0);

  // Test File Write / Read
  const fileContent = "This is a test file.";
  const filePath = "test.txt";

  console.log(`Writing content to '${filePath}'...`);
  await sandbox.write(filePath, fileContent);

  console.log(`Reading content from '${filePath}'...`);
  const readContent = await sandbox.read(filePath);
  expect(readContent.toString("utf-8")).toBe(fileContent);
}

describe("TypeScript SDK E2E", () => {
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

  test("router mode (without warmpool)", async () => {
    deploySandboxTemplate(tc, namespace);
    await deployRouter(tc, namespace);

    const sandbox = new SandboxClient({
      templateName: TEMPLATE_NAME,
      namespace,
    });

    try {
      await sandbox.start();
      console.log("\n--- Running SDK tests without warmpool ---");
      await runSdkTests(sandbox);
      console.log("SDK test without warmpool passed!");
    } finally {
      await sandbox.close();
    }
  });

  test("router mode (with warmpool)", async () => {
    deploySandboxTemplate(tc, namespace);
    await deployRouter(tc, namespace);
    await deployWarmPool(tc, namespace);

    const sandbox = new SandboxClient({
      templateName: TEMPLATE_NAME,
      namespace,
    });

    try {
      await sandbox.start();
      console.log("\n--- Running SDK tests with warmpool ---");
      await runSdkTests(sandbox);
      console.log("SDK test with warmpool passed!");
    } finally {
      await sandbox.close();
    }
  });

  test("gateway mode (without warmpool)", async () => {
    deploySandboxTemplate(tc, namespace);
    await deployRouter(tc, namespace);
    await deployGateway(tc, namespace);

    const sandbox = new SandboxClient({
      templateName: TEMPLATE_NAME,
      namespace,
      gatewayName: GATEWAY_NAME,
      gatewayNamespace: namespace,
    });

    try {
      await sandbox.start();
      console.log("\n--- Running SDK tests without warmpool ---");
      await runSdkTests(sandbox);
      console.log("SDK test without warmpool passed!");
    } finally {
      await sandbox.close();
    }
  });

  test("gateway mode (with warmpool)", async () => {
    deploySandboxTemplate(tc, namespace);
    await deployRouter(tc, namespace);
    await deployWarmPool(tc, namespace);
    await deployGateway(tc, namespace);

    const sandbox = new SandboxClient({
      templateName: TEMPLATE_NAME,
      namespace,
      gatewayName: GATEWAY_NAME,
      gatewayNamespace: namespace,
    });

    try {
      await sandbox.start();
      console.log("\n--- Running SDK tests with warmpool ---");
      await runSdkTests(sandbox);
      console.log("SDK test with warmpool passed!");
    } finally {
      await sandbox.close();
    }
  });
});
