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

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const pkg = JSON.parse(
  readFileSync(new URL("../../package.json", import.meta.url), "utf8"),
) as { scripts: Record<string, string> };

describe("package.json", () => {
  // These scripts ship in the published package and run (or are flagged by
  // npm) on every consumer's install, where tsc is not available.
  it.each([
    "preinstall",
    "install",
    "postinstall",
    "prepare",
  ])("has no %s script", (name) => {
    expect(pkg.scripts).not.toHaveProperty(name);
  });

  it("builds dist before packing", () => {
    expect(pkg.scripts.prepack).toBe("npm run build");
  });
});
