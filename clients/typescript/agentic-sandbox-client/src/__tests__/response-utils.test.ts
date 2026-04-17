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

import { describe, expect, it } from "vitest";
import {
  readBoundedText,
  readBoundedBuffer,
  parseExecutionResult,
  parseFileEntries,
  parseExistsResult,
} from "../response-utils.js";
import {
  SandboxRequestError,
  SandboxResponseTooLargeError,
} from "../exceptions.js";

// --- helpers ---

function makeResponse(body: string, contentLength?: number): Response {
  const headers: Record<string, string> = {
    "Content-Type": "text/plain",
  };
  if (contentLength !== undefined) {
    headers["content-length"] = String(contentLength);
  }
  return new Response(body, { status: 200, headers });
}

function makeBinaryResponse(bytes: Uint8Array): Response {
  return new Response(Buffer.from(bytes), {
    status: 200,
    headers: { "Content-Type": "application/octet-stream" },
  });
}

// --- readBoundedText ---

describe("readBoundedText", () => {
  it("returns text for a normal response within limit", async () => {
    const resp = makeResponse("hello world");
    const text = await readBoundedText(resp, 100, "test");
    expect(text).toBe("hello world");
  });

  it("throws SandboxResponseTooLargeError when Content-Length exceeds limit", async () => {
    const resp = makeResponse("hello world", 50);
    await expect(readBoundedText(resp, 10, "test")).rejects.toBeInstanceOf(
      SandboxResponseTooLargeError,
    );
  });

  it("throws SandboxResponseTooLargeError when stream body exceeds limit", async () => {
    const body = "a".repeat(20);
    const resp = makeResponse(body);
    await expect(readBoundedText(resp, 10, "test")).rejects.toBeInstanceOf(
      SandboxResponseTooLargeError,
    );
  });

  it("includes operationDesc in the error message", async () => {
    const resp = makeResponse("a".repeat(20));
    await expect(readBoundedText(resp, 10, "execute")).rejects.toThrow(
      /execute/,
    );
  });

  it("accepts a body exactly at the limit", async () => {
    const body = "a".repeat(10);
    const resp = makeResponse(body);
    const text = await readBoundedText(resp, 10, "test");
    expect(text).toBe(body);
  });
});

// --- readBoundedBuffer ---

describe("readBoundedBuffer", () => {
  it("returns a Buffer for a normal response within limit", async () => {
    const bytes = new Uint8Array([1, 2, 3, 4]);
    const resp = makeBinaryResponse(bytes);
    const buf = await readBoundedBuffer(resp, 100, "download");
    expect(buf).toBeInstanceOf(Buffer);
    expect(buf.length).toBe(4);
    expect(Array.from(buf)).toEqual([1, 2, 3, 4]);
  });

  it("throws SandboxResponseTooLargeError when body exceeds limit", async () => {
    const bytes = new Uint8Array(20);
    const resp = makeBinaryResponse(bytes);
    await expect(
      readBoundedBuffer(resp, 10, "download"),
    ).rejects.toBeInstanceOf(SandboxResponseTooLargeError);
  });

  it("throws SandboxResponseTooLargeError when response.body is null and body exceeds limit", async () => {
    const bytes = new Uint8Array(20);
    const resp = new Response(Buffer.from(bytes), { status: 200 });
    Object.defineProperty(resp, "body", { value: null, configurable: true });

    await expect(
      readBoundedBuffer(resp, 10, "download"),
    ).rejects.toBeInstanceOf(SandboxResponseTooLargeError);
  });

  it("preserves binary data when response.body is null (non-streaming fallback)", async () => {
    // Bytes that are invalid UTF-8 sequences — text() + TextEncoder round-trip corrupts them
    const binaryBytes = new Uint8Array([0xff, 0xfe, 0x00, 0x80, 0xd8, 0x00]);

    const resp = new Response(Buffer.from(binaryBytes), {
      status: 200,
      headers: { "Content-Type": "application/octet-stream" },
    });

    // Simulate a non-streaming environment: response.body is null
    Object.defineProperty(resp, "body", { value: null, configurable: true });

    // Expected: readBoundedBuffer preserves the exact bytes.
    // Current behavior: fallback path calls response.text() which interprets bytes as UTF-8,
    // then re-encodes via TextEncoder — non-UTF-8 bytes are replaced by U+FFFD (0xEF 0xBF 0xBD).
    const buf = await readBoundedBuffer(resp, 100, "download");
    expect(buf).toBeInstanceOf(Buffer);
    expect(Array.from(buf)).toEqual(Array.from(binaryBytes));
  });
});

// --- parseExecutionResult ---

describe("parseExecutionResult", () => {
  it("parses a valid result", () => {
    const result = parseExecutionResult({
      stdout: "hello",
      stderr: "err",
      exit_code: 0,
    });
    expect(result).toEqual({ stdout: "hello", stderr: "err", exitCode: 0 });
  });

  it("defaults missing stdout to empty string", () => {
    const result = parseExecutionResult({ exit_code: 1 });
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe("");
    expect(result.exitCode).toBe(1);
  });

  it("defaults missing exit_code to -1", () => {
    const result = parseExecutionResult({ stdout: "out" });
    expect(result.exitCode).toBe(-1);
  });

  it("throws SandboxRequestError for null input", () => {
    expect(() => parseExecutionResult(null)).toThrow(SandboxRequestError);
  });

  it("throws SandboxRequestError for string input", () => {
    expect(() => parseExecutionResult("not an object")).toThrow(
      SandboxRequestError,
    );
  });

  it("falls back to defaults for wrong-typed fields", () => {
    const result = parseExecutionResult({
      stdout: 42,
      stderr: null,
      exit_code: "zero",
    });
    expect(result).toEqual({ stdout: "", stderr: "", exitCode: -1 });
  });
});

// --- parseFileEntries ---

describe("parseFileEntries", () => {
  it("parses a valid array", () => {
    const entries = parseFileEntries([
      { name: "foo.txt", size: 100, type: "file", mod_time: 1234 },
      { name: "bar", size: 0, type: "directory", mod_time: 5678 },
    ]);
    expect(entries).toEqual([
      { name: "foo.txt", size: 100, type: "file", modTime: 1234 },
      { name: "bar", size: 0, type: "directory", modTime: 5678 },
    ]);
  });

  it("skips entries with unrecognised type", () => {
    const entries = parseFileEntries([
      { name: "valid.txt", size: 10, type: "file", mod_time: 1 },
      { name: "weird", size: 0, type: "symlink", mod_time: 2 },
    ]);
    expect(entries).toHaveLength(1);
    expect(entries[0].name).toBe("valid.txt");
  });

  it("skips entries missing a string name", () => {
    const entries = parseFileEntries([
      { name: 42, size: 10, type: "file", mod_time: 1 },
      { size: 0, type: "directory", mod_time: 2 },
    ]);
    expect(entries).toHaveLength(0);
  });

  it("throws SandboxRequestError for non-array input", () => {
    expect(() => parseFileEntries({ name: "foo.txt" })).toThrow(
      SandboxRequestError,
    );
  });

  it("throws SandboxRequestError for null input", () => {
    expect(() => parseFileEntries(null)).toThrow(SandboxRequestError);
  });

  it("returns [] for an empty array", () => {
    expect(parseFileEntries([])).toEqual([]);
  });

  it("defaults missing size and mod_time to 0", () => {
    const entries = parseFileEntries([{ name: "f.txt", type: "file" }]);
    expect(entries[0].size).toBe(0);
    expect(entries[0].modTime).toBe(0);
  });
});

// --- parseExistsResult ---

describe("parseExistsResult", () => {
  it("returns true when exists is true", () => {
    expect(parseExistsResult({ exists: true })).toBe(true);
  });

  it("returns false when exists is false", () => {
    expect(parseExistsResult({ exists: false })).toBe(false);
  });

  it("returns false when exists field is missing", () => {
    expect(parseExistsResult({})).toBe(false);
  });

  it("throws SandboxRequestError for non-object input", () => {
    expect(() => parseExistsResult("true")).toThrow(SandboxRequestError);
    expect(() => parseExistsResult(null)).toThrow(SandboxRequestError);
    expect(() => parseExistsResult(1)).toThrow(SandboxRequestError);
  });

  it("returns false when exists is not a boolean", () => {
    expect(parseExistsResult({ exists: "yes" })).toBe(false);
    expect(parseExistsResult({ exists: 1 })).toBe(false);
  });
});
