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

import type { Mock } from "vitest";
import { describe, expect, it, vi } from "vitest";
import { MAX_ERROR_BODY_BYTES } from "../constants.js";
import { SandboxRequestError } from "../exceptions.js";
import { Filesystem } from "../files/filesystem.js";
import type { Span, Tracer } from "../trace-manager.js";

// ---------- helpers ----------

interface FakeSpan extends Span {
  end: Mock;
  setAttribute: Mock;
  recordException: Mock;
  setStatus: Mock;
}

function makeFakeTracer(): {
  tracer: Tracer;
  spans: Array<{ name: string; span: FakeSpan }>;
} {
  const spans: Array<{ name: string; span: FakeSpan }> = [];
  const startActiveSpan = vi.fn((name: string, fn: (span: Span) => unknown) => {
    const span: FakeSpan = {
      isRecording: () => true,
      setAttribute: vi.fn(),
      recordException: vi.fn(),
      setStatus: vi.fn(),
      end: vi.fn(),
    };
    spans.push({ name, span });
    return fn(span);
  });
  const startSpan = vi.fn((name: string) => {
    const span: FakeSpan = {
      isRecording: () => true,
      setAttribute: vi.fn(),
      recordException: vi.fn(),
      setStatus: vi.fn(),
      end: vi.fn(),
    };
    spans.push({ name, span });
    return span;
  });
  const tracer = { startSpan, startActiveSpan } as unknown as Tracer;
  return { tracer, spans };
}

function makeFilesystem(tracer: Tracer | null): Filesystem {
  const requestFn = vi
    .fn()
    .mockResolvedValue(new Response("ok", { status: 200 }));
  return new Filesystem(requestFn, () => tracer, "test-service");
}

// ---------- tests ----------

describe("Filesystem — JSON decode error truncation", () => {
  it("list(): truncates body >MAX_ERROR_BODY_BYTES in the error message", async () => {
    const longBody = "x".repeat(MAX_ERROR_BODY_BYTES + 1);
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response(longBody, { status: 200 }));
    const fs = new Filesystem(requestFn, () => null, "svc");

    const err = await fs.list("/tmp").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain("…");
    expect(err.message).not.toContain(longBody);
  });

  it("list(): does not truncate body <=MAX_ERROR_BODY_BYTES in the error message", async () => {
    const shortBody = "x".repeat(MAX_ERROR_BODY_BYTES);
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response(shortBody, { status: 200 }));
    const fs = new Filesystem(requestFn, () => null, "svc");

    const err = await fs.list("/tmp").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain(shortBody);
    expect(err.message).not.toContain("…");
  });

  it("exists(): truncates body >MAX_ERROR_BODY_BYTES in the error message", async () => {
    const longBody = "x".repeat(MAX_ERROR_BODY_BYTES + 1);
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response(longBody, { status: 200 }));
    const fs = new Filesystem(requestFn, () => null, "svc");

    const err = await fs.exists("/tmp/file.txt").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain("…");
    expect(err.message).not.toContain(longBody);
  });

  it("exists(): does not truncate body <=MAX_ERROR_BODY_BYTES in the error message", async () => {
    const shortBody = "x".repeat(MAX_ERROR_BODY_BYTES);
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response(shortBody, { status: 200 }));
    const fs = new Filesystem(requestFn, () => null, "svc");

    const err = await fs.exists("/tmp/file.txt").catch((e) => e);

    expect(err).toBeInstanceOf(SandboxRequestError);
    expect(err.message).toContain(shortBody);
    expect(err.message).not.toContain("…");
  });
});

describe("Filesystem — safeUploadPath validation", () => {
  function makeFs(): { fs: Filesystem; requestFn: Mock } {
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response("ok", { status: 200 }));
    const fs = new Filesystem(requestFn, () => null, "svc");
    return { fs, requestFn };
  }

  describe("write() path validation", () => {
    it("rejects NUL byte (control character bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs
        .write("foo\x00../etc/passwd", "data")
        .catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("rejects path traversal with ..", async () => {
      const { fs } = makeFs();
      const err = await fs.write("../etc/passwd", "data").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/escapes the sandbox root/);
    });

    it("normalises absolute path by stripping leading slash (matches Python)", async () => {
      // Python's _safe_upload_path strips the leading "/" rather than rejecting.
      const { fs, requestFn } = makeFs();
      await fs.write("/etc/passwd", "data");
      const formData: FormData = requestFn.mock.calls[0][2].body;
      const file = formData.get("file") as File;
      expect(file.name).toBe("etc/passwd");
    });

    it("rejects whitespace-only path", async () => {
      const { fs } = makeFs();
      const err = await fs.write("   ", "data").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/cannot be empty/);
    });

    it("rejects control character 0x1F", async () => {
      const { fs } = makeFs();
      const err = await fs.write("file\x1fname.txt", "data").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("rejects DEL character (0x7F)", async () => {
      const { fs } = makeFs();
      const err = await fs.write("file\x7fname.txt", "data").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("normalises a/./b.txt to a/b.txt and succeeds", async () => {
      const { fs, requestFn } = makeFs();
      await fs.write("a/./b.txt", "data");
      const formData: FormData = requestFn.mock.calls[0][2].body;
      const file = formData.get("file") as File;
      expect(file.name).toBe("a/b.txt");
    });

    it("allows subdirectory paths (relaxed from plain-filename-only)", async () => {
      const { fs, requestFn } = makeFs();
      await fs.write("a/b.txt", "data");
      const formData: FormData = requestFn.mock.calls[0][2].body;
      const file = formData.get("file") as File;
      expect(file.name).toBe("a/b.txt");
    });

    it("skips validation when allowUnsafePaths is true", async () => {
      const { fs } = makeFs();
      await expect(
        fs.write("../etc/passwd", "data", { allowUnsafePaths: true }),
      ).resolves.toBeUndefined();
    });
  });

  describe("read() path validation", () => {
    it("rejects NUL byte (control character bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.read("foo\x00../etc/passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("rejects path traversal with ..", async () => {
      const { fs } = makeFs();
      const err = await fs.read("../etc/passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/escapes the sandbox root/);
    });

    it("rejects backslash-style traversal (Windows path bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.read("..\\etc\\passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/forward slashes/);
    });

    it("normalises absolute path by stripping leading slash (matches Python)", async () => {
      // Python's _safe_upload_path strips the leading "/" rather than rejecting.
      const { fs, requestFn } = makeFs();
      requestFn.mockResolvedValue(
        new Response(new Uint8Array([1]).buffer, { status: 200 }),
      );
      await fs.read("/etc/passwd");
      const endpoint: string = requestFn.mock.calls[0][1];
      expect(endpoint).toBe("download/etc%2Fpasswd");
    });

    it("rejects whitespace-only path", async () => {
      const { fs } = makeFs();
      const err = await fs.read("   ").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/cannot be empty/);
    });

    it("normalises a/./b.txt and encodes the request URL correctly", async () => {
      const { fs, requestFn } = makeFs();
      const mockResponse = new Response(new Uint8Array([1, 2, 3]).buffer, {
        status: 200,
      });
      requestFn.mockResolvedValue(mockResponse);
      await fs.read("a/./b.txt");
      const endpoint: string = requestFn.mock.calls[0][1];
      expect(endpoint).toBe("download/a%2Fb.txt");
    });

    it("skips validation when allowUnsafePaths is true", async () => {
      const { fs } = makeFs();
      await expect(
        fs.read("../etc/passwd", { allowUnsafePaths: true }),
      ).resolves.toBeInstanceOf(Buffer);
    });
  });

  describe("list() path validation", () => {
    it("rejects path traversal with ..", async () => {
      const { fs } = makeFs();
      const err = await fs.list("../secrets").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/escapes the sandbox root/);
    });

    it("rejects NUL byte (control character bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.list("foo\x00../secrets").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("rejects backslash-style traversal (Windows path bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.list("..\\secrets").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/forward slashes/);
    });

    it("rejects whitespace-only path even with allowUnsafePaths: true", async () => {
      const { fs } = makeFs();
      const err = await fs
        .list("   ", { allowUnsafePaths: true })
        .catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/cannot be empty/);
    });

    it("skips validation when allowUnsafePaths is true", async () => {
      const { fs, requestFn } = makeFs();
      requestFn.mockResolvedValue(
        new Response(JSON.stringify([]), { status: 200 }),
      );
      await expect(
        fs.list("../secrets", { allowUnsafePaths: true }),
      ).resolves.toEqual([]);
    });
  });

  describe("exists() path validation", () => {
    it("rejects path traversal with ..", async () => {
      const { fs } = makeFs();
      const err = await fs.exists("../etc/passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/escapes the sandbox root/);
    });

    it("rejects NUL byte (control character bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.exists("foo\x00../etc/passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/ASCII control characters/);
    });

    it("rejects backslash-style traversal (Windows path bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.exists("..\\etc\\passwd").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/forward slashes/);
    });

    it("rejects whitespace-only path even with allowUnsafePaths: true", async () => {
      const { fs } = makeFs();
      const err = await fs
        .exists("   ", { allowUnsafePaths: true })
        .catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/cannot be empty/);
    });

    it("skips validation when allowUnsafePaths is true", async () => {
      const { fs, requestFn } = makeFs();
      requestFn.mockResolvedValue(
        new Response(JSON.stringify({ exists: true }), { status: 200 }),
      );
      await expect(
        fs.exists("../etc/passwd", { allowUnsafePaths: true }),
      ).resolves.toBe(true);
    });
  });

  describe("write() path validation (backslash)", () => {
    it("rejects backslash-style traversal (Windows path bypass)", async () => {
      const { fs } = makeFs();
      const err = await fs.write("..\\etc\\passwd", "data").catch((e) => e);
      expect(err).toBeInstanceOf(Error);
      expect(err.message).toMatch(/forward slashes/);
    });
  });
});

describe("Filesystem — sandbox.file.path span records normalized path", () => {
  it("write(): records normalized path (not original) in span", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);

    await fs.write("/sub/file.txt", "data");

    const span = spans[0].span;
    // Path is set twice: original before validation, normalized after.
    const pathCall = span.setAttribute.mock.calls.findLast(
      (args) => args[0] === "sandbox.file.path",
    );
    expect(pathCall?.[1]).toBe("sub/file.txt");
  });

  it("read(): records normalized path (not original) in span", async () => {
    const { tracer, spans } = makeFakeTracer();
    const requestFn = vi
      .fn()
      .mockResolvedValue(
        new Response(new Uint8Array([1, 2, 3]).buffer, { status: 200 }),
      );
    const fs = new Filesystem(requestFn, () => tracer, "test-service");

    await fs.read("/sub/file.txt");

    const span = spans[0].span;
    const pathCall = span.setAttribute.mock.calls.findLast(
      (args) => args[0] === "sandbox.file.path",
    );
    expect(pathCall?.[1]).toBe("sub/file.txt");
  });

  it("list(): records normalized path (not original) in span", async () => {
    const { tracer, spans } = makeFakeTracer();
    const requestFn = vi
      .fn()
      .mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }));
    const fs = new Filesystem(requestFn, () => tracer, "test-service");

    await fs.list("/sub/dir");

    const span = spans[0].span;
    const pathCall = span.setAttribute.mock.calls.findLast(
      (args) => args[0] === "sandbox.file.path",
    );
    expect(pathCall?.[1]).toBe("sub/dir");
  });

  it("exists(): records normalized path (not original) in span", async () => {
    const { tracer, spans } = makeFakeTracer();
    const requestFn = vi
      .fn()
      .mockResolvedValue(
        new Response(JSON.stringify({ exists: false }), { status: 200 }),
      );
    const fs = new Filesystem(requestFn, () => tracer, "test-service");

    await fs.exists("/sub/file.txt");

    const span = spans[0].span;
    const pathCall = span.setAttribute.mock.calls.findLast(
      (args) => args[0] === "sandbox.file.path",
    );
    expect(pathCall?.[1]).toBe("sub/file.txt");
  });

  it("write(): records original path in span even when validation fails", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);

    await fs.write("../etc/passwd", "data").catch(() => {});

    const span = spans[0].span;
    const pathCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.path",
    );
    expect(pathCall?.[1]).toBe("../etc/passwd");
  });
});

describe("Filesystem.write — sandbox.file.size trace attribute", () => {
  it("records byte length for ASCII string content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);

    await fs.write("file.txt", "hello");

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBe(5);
  });

  it("records byte length (not character count) for non-ASCII string content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);
    const content = "こんにちは"; // 5 chars, 15 bytes in UTF-8

    await fs.write("file.txt", content);

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBeGreaterThan(content.length);
    expect(sizeCall?.[1]).toBe(Buffer.byteLength(content));
  });

  it("records byte length for Buffer content", async () => {
    const { tracer, spans } = makeFakeTracer();
    const fs = makeFilesystem(tracer);
    const buf = Buffer.from([0x01, 0x02, 0x03]);

    await fs.write("file.bin", buf);

    const span = spans[0].span;
    const sizeCall = span.setAttribute.mock.calls.find(
      (args) => args[0] === "sandbox.file.size",
    );
    expect(sizeCall?.[1]).toBe(3);
  });
});
