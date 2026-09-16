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

import * as http from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import {
  SandboxConnectionError,
  type SandboxdApiError,
  SandboxError,
} from "../exceptions.js";
import {
  resolveSandboxPath,
  SandboxdRestClient,
  SourceFailure,
} from "../rest.js";

// ---------- test HTTP server harness ----------

interface RecordedRequest {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
  body: Buffer;
}

async function startServer(
  handler: (req: http.IncomingMessage, res: http.ServerResponse) => void,
): Promise<{
  baseUrl: string;
  requests: RecordedRequest[];
  close(): Promise<void>;
}> {
  const requests: RecordedRequest[] = [];
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      requests.push({
        method: req.method ?? "",
        url: req.url ?? "",
        headers: req.headers,
        body: Buffer.concat(chunks),
      });
      handler(req, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("failed to bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    requests,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

function makeClient(
  baseUrl: string,
  overrides: Partial<{
    maxDownloadSize: number;
    maxUploadSize: number;
    maxMetadataResponseSize: number;
  }> = {},
): SandboxdRestClient {
  return new SandboxdRestClient({
    baseUrl,
    maxDownloadSize: overrides.maxDownloadSize ?? 1024 * 1024,
    maxUploadSize: overrides.maxUploadSize ?? 1024 * 1024,
    maxMetadataResponseSize: overrides.maxMetadataResponseSize ?? 1024 * 1024,
  });
}

let activeServer: Awaited<ReturnType<typeof startServer>> | undefined;

afterEach(async () => {
  await activeServer?.close();
  activeServer = undefined;
});

// ---------- resolveSandboxPath (client-side validation) ----------

describe("resolveSandboxPath", () => {
  it("maps '' and '.' to the sandbox root for read/list/exists", () => {
    expect(resolveSandboxPath("", "read")).toBe("");
    expect(resolveSandboxPath(".", "list")).toBe("");
    expect(resolveSandboxPath("./.", "exists")).toBe("");
  });

  it("rejects the sandbox root for write/delete", () => {
    expect(() => resolveSandboxPath("", "write")).toThrow(SandboxError);
    expect(() => resolveSandboxPath(".", "delete")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("./", "write")).toThrow(SandboxError);
  });

  it("rejects a trailing slash for write/delete but not read/list/exists", () => {
    expect(() => resolveSandboxPath("dir/", "write")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("dir/", "delete")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("dir/", "list")).not.toThrow();
  });

  it("rejects absolute paths for every operation", () => {
    for (const op of ["read", "write", "exists", "list", "delete"] as const) {
      expect(() => resolveSandboxPath("/etc/passwd", op)).toThrow(SandboxError);
    }
  });

  it("rejects any '..' segment for every operation, including exists", () => {
    expect(() => resolveSandboxPath("..", "exists")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("a/../b", "read")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("a/b/..", "list")).toThrow(SandboxError);
  });

  it("does NOT treat a literal '%2E%2E' as a parent segment", () => {
    // Not decoded/normalized before validation: this is a literal filename.
    expect(() => resolveSandboxPath("%2E%2E", "read")).not.toThrow();
    const encoded = resolveSandboxPath("%2E%2E", "read");
    expect(encoded).toBe(encodeURIComponent("%2E%2E"));
  });

  it("rejects a NUL byte and unpaired surrogates", () => {
    expect(() => resolveSandboxPath("a\0b", "read")).toThrow(SandboxError);
    expect(() => resolveSandboxPath("a\ud800b", "read")).toThrow(SandboxError);
  });

  it("encodes '/', '%', space, '?', and '#' via a single encodeURIComponent pass", () => {
    expect(resolveSandboxPath("dir/sub file.txt", "read")).toBe(
      encodeURIComponent("dir/sub file.txt"),
    );
    expect(resolveSandboxPath("a?b#c%d", "read")).toBe(
      encodeURIComponent("a?b#c%d"),
    );
  });
});

// ---------- health ----------

describe("SandboxdRestClient.health", () => {
  it("returns status/uptime on a valid 200 response", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "ok", uptime_seconds: 42 }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(client.health(new AbortController().signal)).resolves.toEqual({
      status: "ok",
      uptimeSeconds: 42,
    });
  });

  it("throws SandboxdApiError with status 503 when unhealthy", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(503, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({ code: "UNAVAILABLE", message: "shutting down" }),
      );
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.health(new AbortController().signal),
    ).rejects.toMatchObject({
      status: 503,
    } satisfies Partial<SandboxdApiError>);
  });

  it("throws on malformed JSON without retry semantics baked in", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end("not json");
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(client.health(new AbortController().signal)).rejects.toThrow(
      SandboxError,
    );
  });

  it("throws on an unexpected body shape", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ status: "ok", uptime_seconds: -1 }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(client.health(new AbortController().signal)).rejects.toThrow(
      SandboxError,
    );
  });
});

// ---------- read ----------

describe("SandboxdRestClient.read", () => {
  it("GETs the exact encoded req.url and returns the raw body", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.end(Buffer.from("hello"));
    });
    const client = makeClient(activeServer.baseUrl);
    const result = await client.read(
      "dir/sub file.txt",
      new AbortController().signal,
    );
    expect(new TextDecoder().decode(result)).toBe("hello");
    expect(activeServer.requests[0].method).toBe("GET");
    expect(activeServer.requests[0].url).toBe(
      `/v1/files/${encodeURIComponent("dir/sub file.txt")}`,
    );
  });

  it("requests '/v1/files/' for the sandbox root", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ path: "/", entries: [] }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(client.read("", new AbortController().signal)).rejects.toThrow(
      SandboxError,
    );
    expect(activeServer.requests[0].url).toBe("/v1/files/");
  });

  it("errors when the response is a directory listing (JSON content-type)", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json; charset=utf-8" });
      res.end(JSON.stringify({ path: "/dir", entries: [] }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.read("dir", new AbortController().signal),
    ).rejects.toThrow(SandboxError);
  });

  it("maps a 404 to SandboxdApiError with code NOT_FOUND", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(404, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ code: "NOT_FOUND", message: "nope" }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.read("missing.txt", new AbortController().signal),
    ).rejects.toMatchObject({
      status: 404,
      code: "NOT_FOUND",
    } satisfies Partial<SandboxdApiError>);
  });

  it("aborts and rejects once the download exceeds maxDownloadSize", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.alloc(10));
      // Keep the connection open a bit before finishing to give the client
      // time to observe the overflow and abort.
      setTimeout(() => {
        try {
          res.end(Buffer.alloc(10));
        } catch {
          // client already aborted the underlying socket
        }
      }, 20);
    });
    const client = makeClient(activeServer.baseUrl, { maxDownloadSize: 5 });
    await expect(
      client.read("big.bin", new AbortController().signal),
    ).rejects.toThrow(SandboxError);
  });
});

// ---------- readStream ----------

async function drain(stream: ReadableStream<Uint8Array>): Promise<Uint8Array> {
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
  for (const c of chunks) {
    out.set(c, offset);
    offset += c.byteLength;
  }
  return out;
}

describe("SandboxdRestClient.readStream", () => {
  it("streams chunks that concatenate to the exact original content", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("hello "));
      res.write(Buffer.from("world"));
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = await client.readStream(
      "big.bin",
      new AbortController().signal,
    );
    const result = await drain(stream);
    expect(new TextDecoder().decode(result)).toBe("hello world");
  });

  it("resolves an already-closed stream for an empty file", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = await client.readStream(
      "empty.bin",
      new AbortController().signal,
    );
    const reader = stream.getReader();
    const { done, value } = await reader.read();
    expect(done).toBe(true);
    expect(value).toBeUndefined();
  });

  it("rejects before resolving when the response is a directory listing", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ path: "/dir", entries: [] }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.readStream("dir", new AbortController().signal),
    ).rejects.toThrow(SandboxError);
  });

  it("rejects before resolving on a 404", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(404, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ code: "NOT_FOUND", message: "nope" }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.readStream("missing.txt", new AbortController().signal),
    ).rejects.toMatchObject({
      status: 404,
    } satisfies Partial<SandboxdApiError>);
  });

  it("errors the stream once downloaded bytes exceed maxDownloadSize, without buffering past the limit", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.alloc(10));
      setTimeout(() => {
        try {
          res.end(Buffer.alloc(10));
        } catch {
          // client already aborted the underlying socket
        }
      }, 20);
    });
    const client = makeClient(activeServer.baseUrl, { maxDownloadSize: 5 });
    const stream = await client.readStream(
      "big.bin",
      new AbortController().signal,
    );
    const reader = stream.getReader();
    await expect(reader.read()).rejects.toThrow(SandboxError);
  });

  it("ends cleanly, without error, when the consumer cancels", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      // Never call res.end(): the stream only ends because the consumer
      // cancels it.
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = await client.readStream(
      "a.txt",
      new AbortController().signal,
    );
    const reader = stream.getReader();
    await reader.read();
    await expect(reader.cancel()).resolves.toBeUndefined();
  });

  it("errors with the caller's own signal reason once it fires mid-stream", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      // Never call res.end(): the read hangs until the caller's signal fires.
    });
    const client = makeClient(activeServer.baseUrl);
    const controller = new AbortController();
    const stream = await client.readStream("a.txt", controller.signal);
    const reader = stream.getReader();
    await reader.read();
    const reason = new Error("caller timeout");
    setTimeout(() => controller.abort(reason), 30);
    await expect(reader.read()).rejects.toBe(reason);
  });

  it("errors even when the consumer never reads again after the abort (queue-full/stalled case)", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("first-chunk"));
      // Never call res.end().
    });
    const client = makeClient(activeServer.baseUrl);
    const controller = new AbortController();
    const stream = await client.readStream("a.txt", controller.signal);
    const reader = stream.getReader();
    const reason = new Error("caller timeout");
    controller.abort(reason);
    // No further read() is issued before the abort; the stream must still
    // observe it via the underlying reader's `closed` rejection.
    await expect(reader.closed).rejects.toBe(reason);
  });

  it("wraps a mid-body disconnect as SandboxConnectionError", async () => {
    activeServer = await startServer((req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      // Delay the reset so headers + the first chunk are reliably delivered
      // before the socket dies — otherwise this can race the initial
      // request() and fail there instead of exercising the mid-stream path.
      setTimeout(() => req.socket.destroy(), 20);
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = await client.readStream(
      "a.txt",
      new AbortController().signal,
    );
    const reader = stream.getReader();
    await reader.read();
    await expect(reader.read()).rejects.toBeInstanceOf(SandboxConnectionError);
  });
});

// ---------- write ----------

describe("SandboxdRestClient.write", () => {
  it("PUTs octet-stream content and succeeds on 204", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await client.write(
      "a.txt",
      new TextEncoder().encode("hi"),
      {},
      new AbortController().signal,
    );
    expect(activeServer.requests[0].method).toBe("PUT");
    expect(activeServer.requests[0].headers["content-type"]).toBe(
      "application/octet-stream",
    );
    expect(activeServer.requests[0].body.toString()).toBe("hi");
  });

  it("rejects an invalid mode before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.write(
        "a.txt",
        new Uint8Array(),
        { mode: "777" },
        new AbortController().signal,
      ),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });

  it("rejects content over maxUploadSize before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl, { maxUploadSize: 4 });
    await expect(
      client.write(
        "a.txt",
        new Uint8Array(10),
        {},
        new AbortController().signal,
      ),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });

  it("rejects writing to the sandbox root before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.write("", new Uint8Array(), {}, new AbortController().signal),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });

  it("sends the mode as a query parameter", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await client.write(
      "a.txt",
      new Uint8Array(),
      { mode: "0755" },
      new AbortController().signal,
    );
    expect(activeServer.requests[0].url).toBe("/v1/files/a.txt?mode=0755");
  });
});

// ---------- writeStream ----------

function toReadableStream(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i >= chunks.length) {
        controller.close();
        return;
      }
      controller.enqueue(chunks[i++]);
    },
  });
}

function textChunks(...parts: string[]): Uint8Array[] {
  return parts.map((p) => new TextEncoder().encode(p));
}

/** Like startServer(), but also reports live (pre-`end`) received bytes so
 * an aborted/cancelled upload's partial delivery is observable — the
 * request-recording `end` handler never fires for those. */
async function startServerWithLiveByteCount(
  handler: (req: http.IncomingMessage, res: http.ServerResponse) => void,
): Promise<{
  baseUrl: string;
  liveBytes(): number;
  ended(): boolean;
  close(): Promise<void>;
}> {
  let liveBytes = 0;
  let didEnd = false;
  const server = http.createServer((req, res) => {
    req.on("data", (c: Buffer) => {
      liveBytes += c.length;
    });
    req.on("end", () => {
      didEnd = true;
    });
    handler(req, res);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("failed to bind");
  return {
    baseUrl: `http://127.0.0.1:${addr.port}`,
    liveBytes: () => liveBytes,
    ended: () => didEnd,
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

describe("SandboxdRestClient.writeStream", () => {
  it("PUTs chunked content that concatenates to exactly the source and returns the byte count", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    const bytesSent = await client.writeStream(
      "a.txt",
      toReadableStream(textChunks("hello ", "world")),
      {},
      new AbortController().signal,
    );
    expect(bytesSent).toBe(11);
    expect(activeServer.requests[0].method).toBe("PUT");
    expect(activeServer.requests[0].headers["content-type"]).toBe(
      "application/octet-stream",
    );
    expect(activeServer.requests[0].body.toString()).toBe("hello world");
  });

  it("sends the mode as a query parameter", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await client.writeStream(
      "a.txt",
      toReadableStream([]),
      { mode: "0755" },
      new AbortController().signal,
    );
    expect(activeServer.requests[0].url).toBe("/v1/files/a.txt?mode=0755");
  });

  it("rejects an invalid mode before ever touching the source stream", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = toReadableStream(textChunks("x"));
    await expect(
      client.writeStream(
        "a.txt",
        stream,
        { mode: "777" },
        new AbortController().signal,
      ),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
    expect(stream.locked).toBe(false);
  });

  it("rejects an already-locked stream before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    const stream = toReadableStream(textChunks("x"));
    stream.getReader();
    await expect(
      client.writeStream("a.txt", stream, {}, new AbortController().signal),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });

  it("rejects writing to the sandbox root before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.writeStream(
        "",
        toReadableStream([]),
        {},
        new AbortController().signal,
      ),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });

  it("maps a non-204 response to SandboxdApiError and cancels the source", async () => {
    // startServer() only invokes its handler once the request body has
    // ended, which would deadlock here since the source below never
    // completes on its own — use the immediate-handler helper instead, and
    // force the connection closed so afterEach()/this test's own cleanup
    // never waits on a lingering keep-alive socket.
    const server = await startServerWithLiveByteCount((_req, res) => {
      res.writeHead(400, {
        "Content-Type": "application/json",
        Connection: "close",
      });
      res.end(JSON.stringify({ code: "INVALID_ARGUMENT", message: "bad" }));
    });
    const client = makeClient(server.baseUrl);
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      async pull(controller) {
        await new Promise((r) => setTimeout(r, 10));
        controller.enqueue(new Uint8Array(4));
      },
      cancel() {
        cancelled = true;
      },
    });
    try {
      await expect(
        client.writeStream("a.txt", stream, {}, new AbortController().signal),
      ).rejects.toMatchObject({
        status: 400,
      } satisfies Partial<SandboxdApiError>);
      expect(cancelled).toBe(true);
    } finally {
      await server.close();
    }
  });

  it("treats an early 204 (before the source reaches EOF) as invalid_response and cancels the source", async () => {
    const server = await startServerWithLiveByteCount((_req, res) => {
      // Reply before consuming the body at all.
      res.writeHead(204, { Connection: "close" });
      res.end();
    });
    const client = makeClient(server.baseUrl);
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      async pull(controller) {
        await new Promise((r) => setTimeout(r, 20));
        controller.enqueue(new Uint8Array(4));
      },
      cancel() {
        cancelled = true;
      },
    });
    try {
      await expect(
        client.writeStream("a.txt", stream, {}, new AbortController().signal),
      ).rejects.toThrow(SandboxError);
      expect(cancelled).toBe(true);
    } finally {
      await server.close();
    }
  });

  it("succeeds when content is exactly maxUploadSize bytes", async () => {
    const server = await startServerWithLiveByteCount((req, res) => {
      // Only respond once the full body has arrived — an early response
      // would (correctly) be rejected as invalid_response instead.
      req.on("end", () => {
        res.writeHead(204, { Connection: "close" });
        res.end();
      });
    });
    const client = makeClient(server.baseUrl, { maxUploadSize: 10 });
    try {
      const bytesSent = await client.writeStream(
        "a.txt",
        toReadableStream([new Uint8Array(10)]),
        {},
        new AbortController().signal,
      );
      expect(bytesSent).toBe(10);
    } finally {
      await server.close();
    }
  });

  it("rejects content over maxUploadSize without the server ever receiving more than the limit", async () => {
    const server = await startServerWithLiveByteCount((req, res) => {
      req.on("end", () => {
        res.writeHead(204);
        res.end();
      });
    });
    const client = makeClient(server.baseUrl, { maxUploadSize: 10 });
    try {
      await expect(
        client.writeStream(
          "a.txt",
          toReadableStream([
            new Uint8Array(6),
            new Uint8Array(6),
            new Uint8Array(6),
          ]),
          {},
          new AbortController().signal,
        ),
      ).rejects.toMatchObject({ telemetryCode: "request_too_large" });
      // Give the abort a moment to propagate; the server must never see the
      // full 18-byte payload, nor a completed request.
      await new Promise((r) => setTimeout(r, 50));
      expect(server.liveBytes()).toBeLessThanOrEqual(10);
      expect(server.ended()).toBe(false);
    } finally {
      await server.close();
    }
  });

  it("rejects when the very first chunk already exceeds maxUploadSize", async () => {
    const server = await startServerWithLiveByteCount((req, res) => {
      req.on("end", () => {
        res.writeHead(204);
        res.end();
      });
    });
    const client = makeClient(server.baseUrl, { maxUploadSize: 4 });
    try {
      await expect(
        client.writeStream(
          "a.txt",
          toReadableStream([new Uint8Array(10)]),
          {},
          new AbortController().signal,
        ),
      ).rejects.toMatchObject({ telemetryCode: "request_too_large" });
    } finally {
      await server.close();
    }
  });

  it("tags a source stream failure as SourceFailure, preserving the original thrown value (including undefined)", async () => {
    activeServer = await startServer((req, res) => {
      req.on("data", () => {});
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);

    const boom = new Error("source blew up");
    const throwingStream = (value: unknown): ReadableStream<Uint8Array> =>
      new ReadableStream<Uint8Array>({
        pull() {
          throw value;
        },
      });

    const p1 = client.writeStream(
      "a.txt",
      throwingStream(boom),
      {},
      new AbortController().signal,
    );
    await expect(p1).rejects.toBeInstanceOf(SourceFailure);
    await p1.catch((err: unknown) => {
      expect((err as SourceFailure).value).toBe(boom);
    });

    const p2 = client.writeStream(
      "a.txt",
      // eslint-disable-next-line no-throw-literal
      throwingStream(undefined),
      {},
      new AbortController().signal,
    );
    await expect(p2).rejects.toBeInstanceOf(SourceFailure);
    await p2.catch((err: unknown) => {
      expect((err as SourceFailure).value).toBeUndefined();
    });

    const connErr = new SandboxConnectionError("boom", "socket");
    const p3 = client.writeStream(
      "a.txt",
      throwingStream(connErr),
      {},
      new AbortController().signal,
    );
    await expect(p3).rejects.toBeInstanceOf(SourceFailure);
    await p3.catch((err: unknown) => {
      expect((err as SourceFailure).value).toBe(connErr);
    });
  });
});

// ---------- exists ----------

describe("SandboxdRestClient.exists", () => {
  it("returns true on 2xx and issues a HEAD request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.exists("a.txt", new AbortController().signal),
    ).resolves.toBe(true);
    expect(activeServer.requests[0].method).toBe("HEAD");
  });

  it("returns false on 404", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(404);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.exists("missing.txt", new AbortController().signal),
    ).resolves.toBe(false);
  });

  it("throws on other statuses", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(403);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.exists("a.txt", new AbortController().signal),
    ).rejects.toMatchObject({
      status: 403,
    } satisfies Partial<SandboxdApiError>);
  });
});

// ---------- list ----------

describe("SandboxdRestClient.list", () => {
  it("parses a valid directory listing", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({
          path: "/dir",
          entries: [
            {
              name: "a.txt",
              size: 5,
              type: "file",
              modified_at: "2024-01-02T03:04:05Z",
              mode: "0644",
            },
            {
              name: "sub",
              size: 0,
              type: "directory",
              modified_at: "2024-01-02T03:04:05Z",
            },
          ],
        }),
      );
    });
    const client = makeClient(activeServer.baseUrl);
    const listing = await client.list("dir", new AbortController().signal);
    expect(listing.path).toBe("/dir");
    expect(listing.entries).toHaveLength(2);
    expect(listing.entries[0]).toEqual({
      name: "a.txt",
      size: 5,
      type: "file",
      modifiedAt: "2024-01-02T03:04:05Z",
      mode: "0644",
    });
  });

  it("skips unknown entry types without throwing", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({
          path: "/dir",
          entries: [
            {
              name: "link",
              size: 0,
              type: "symlink",
              modified_at: "2024-01-02T03:04:05Z",
            },
            {
              name: "a.txt",
              size: 1,
              type: "file",
              modified_at: "2024-01-02T03:04:05Z",
            },
          ],
        }),
      );
    });
    const client = makeClient(activeServer.baseUrl);
    const listing = await client.list("dir", new AbortController().signal);
    expect(listing.entries.map((e) => e.name)).toEqual(["a.txt"]);
  });

  it("rejects an invalid modified_at (non-existent calendar date)", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({
          path: "/dir",
          entries: [
            {
              name: "a.txt",
              size: 1,
              type: "file",
              modified_at: "2024-02-30T00:00:00Z",
            },
          ],
        }),
      );
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.list("dir", new AbortController().signal),
    ).rejects.toThrow(SandboxError);
  });

  it("errors when the target is a file, not a directory", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.end("raw file contents");
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.list("a.txt", new AbortController().signal),
    ).rejects.toThrow(SandboxError);
  });
});

// ---------- delete ----------

describe("SandboxdRestClient.delete", () => {
  it("DELETEs and succeeds on 204", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await client.delete("a.txt", {}, new AbortController().signal);
    expect(activeServer.requests[0].method).toBe("DELETE");
    expect(activeServer.requests[0].url).toBe("/v1/files/a.txt");
  });

  it("sends recursive=true only when requested", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await client.delete(
      "dir",
      { recursive: true },
      new AbortController().signal,
    );
    expect(activeServer.requests[0].url).toBe("/v1/files/dir?recursive=true");
  });

  it("maps a 409 to SandboxdApiError with code CONFLICT", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ code: "CONFLICT", message: "not empty" }));
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.delete("dir", {}, new AbortController().signal),
    ).rejects.toMatchObject({
      status: 409,
      code: "CONFLICT",
    } satisfies Partial<SandboxdApiError>);
  });

  it("rejects deleting the sandbox root before sending any request", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(204);
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.delete("", {}, new AbortController().signal),
    ).rejects.toThrow(SandboxError);
    expect(activeServer.requests).toHaveLength(0);
  });
});

// ---------- redirects & network failure ----------

describe("SandboxdRestClient network behavior", () => {
  it("treats a redirect as a failure rather than following it", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(302, { Location: "/somewhere-else" });
      res.end();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.read("a.txt", new AbortController().signal),
    ).rejects.toThrow();
  });

  it("wraps a connection failure as SandboxConnectionError", async () => {
    // Nothing is listening on this port.
    const client = makeClient("http://127.0.0.1:1");
    await expect(
      client.read("a.txt", new AbortController().signal),
    ).rejects.toBeInstanceOf(SandboxConnectionError);
  });

  it("aborts a stalled body read once the caller's signal fires mid-stream", async () => {
    activeServer = await startServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      // Never call res.end(): the body read hangs until the caller's own
      // signal (not just the size-overflow controller) tears it down.
    });
    const client = makeClient(activeServer.baseUrl);
    const controller = new AbortController();
    const reason = new Error("caller timeout");
    setTimeout(() => controller.abort(reason), 50);
    await expect(client.read("a.txt", controller.signal)).rejects.toBe(reason);
  });

  it("wraps a mid-body disconnect (no caller abort) as SandboxConnectionError", async () => {
    activeServer = await startServer((req, res) => {
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.write(Buffer.from("partial"));
      // The peer resets the connection after sending headers and some body
      // bytes — nobody aborted the caller's own signal.
      req.socket.destroy();
    });
    const client = makeClient(activeServer.baseUrl);
    await expect(
      client.read("a.txt", new AbortController().signal),
    ).rejects.toBeInstanceOf(SandboxConnectionError);
  });
});
