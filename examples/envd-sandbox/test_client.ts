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

/**
 * Verification script for the envd-sandbox example.
 * Drives the envd REST API endpoints using fetch.
 *
 * Run:
 *   SANDBOX_BASE_URL=http://127.0.0.1:49983 npx tsx test_client.ts
 */

/* eslint-disable @typescript-eslint/no-explicit-any */
declare var process: any;

interface TestResult {
  name: string;
  passed: boolean;
  detail: string;
}

async function runTest(
  name: string,
  fn: () => Promise<string>,
): Promise<TestResult> {
  const start = Date.now();
  try {
    const detail = await fn();
    const elapsed = ((Date.now() - start) / 1000).toFixed(2);
    return { name, passed: true, detail: `${detail} (${elapsed}s)` };
  } catch (err: any) {
    return { name, passed: false, detail: `${err.message}` };
  }
}

async function main(): Promise<number> {
  const baseURL = process.env.SANDBOX_BASE_URL;
  if (!baseURL) {
    console.error(
      "SANDBOX_BASE_URL is not set. Point it at a running envd sandbox pod,",
    );
    console.error(
      "e.g. via `kubectl port-forward pod/<name> 49983:49983` and export",
    );
    console.error("SANDBOX_BASE_URL=http://127.0.0.1:49983");
    return 1;
  }

  const url = baseURL.replace(/\/+$/, "");

  const tests: { name: string; fn: () => Promise<string> }[] = [
    {
      name: "health",
      fn: async () => {
        const r = await fetch(`${url}/health`, {
          signal: AbortSignal.timeout(10000),
        });
        if (r.status !== 204) throw new Error(`expected 204, got ${r.status}`);
        return "204 No Content";
      },
    },
    {
      name: "init",
      fn: async () => {
        const r = await fetch(`${url}/init`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            envVars: { HELLO: "envd" },
            defaultUser: "user",
          }),
          signal: AbortSignal.timeout(10000),
        });
        // envd returns 204 No Content on success in --isnotfc mode.
        if (r.status !== 204) {
          throw new Error(`init: expected 204, got ${r.status}`);
        }
        return "init ok";
      },
    },
    {
      name: "files",
      fn: async () => {
        const formData = new FormData();
        formData.append("path", "hello.txt");
        formData.append(
          "file",
          new Blob(["hi from envd-sandbox"], { type: "text/plain" }),
          "hello.txt",
        );
        const r = await fetch(`${url}/files`, {
          method: "POST",
          body: formData,
          signal: AbortSignal.timeout(10000),
        });
        if (r.status !== 200) {
          const text = await r.text();
          throw new Error(`upload: expected 200, got ${r.status}: ${text}`);
        }
        const r2 = await fetch(`${url}/files?path=hello.txt`, {
          signal: AbortSignal.timeout(10000),
        });
        const content = await r2.text();
        if (content !== "hi from envd-sandbox") {
          throw new Error(`content mismatch: ${content}`);
        }
        return "round-trip ok";
      },
    },
    {
      name: "metrics",
      fn: async () => {
        const r = await fetch(`${url}/metrics`, {
          signal: AbortSignal.timeout(10000),
        });
        if (r.status !== 200) {
          const text = await r.text();
          throw new Error(`expected 200, got ${r.status}: ${text}`);
        }
        const body = (await r.json()) as Record<string, unknown>;
        if (!("ts" in body) && !("cpu_count" in body)) {
          throw new Error(`unexpected metrics: ${JSON.stringify(body)}`);
        }
        return `keys=${Object.keys(body).sort().join(",")}`;
      },
    },
  ];

  console.log("\n=== envd-sandbox verification: TypeScript fetch ===\n");

  const results: TestResult[] = [];
  for (let i = 0; i < tests.length; i++) {
    const t = tests[i];
    console.log(`[${i + 1}/${tests.length}] running ${t.name}...`);
    const result = await runTest(t.name, t.fn);
    results.push(result);
    const status = result.passed ? "PASS" : "FAIL";
    console.log(`         ${status}: ${result.detail}`);
  }

  console.log("\n=== Summary ===");
  const failed = results.filter((r) => !r.passed);
  for (const r of results) {
    const mark = r.passed ? "PASS" : "FAIL";
    console.log(`  [${mark}] ${r.name}: ${r.detail}`);
  }

  if (failed.length > 0) {
    console.log(`\n${failed.length} test(s) failed.`);
    return 1;
  }
  console.log(`\nAll ${results.length} test(s) passed.`);
  return 0;
}

main().then((code) => process.exit(code));
