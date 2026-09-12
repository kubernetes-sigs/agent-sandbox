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
import { resolveSandboxPath, SandboxdRestClient } from "../rest.js";

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
