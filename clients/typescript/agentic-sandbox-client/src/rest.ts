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

import { ERROR_DETAIL_MAX_BYTES } from "./constants.js";
import {
  SandboxConnectionError,
  type SandboxdApiCode,
  SandboxdApiError,
  SandboxError,
} from "./exceptions.js";
import type {
  DirectoryListing,
  FileEntry,
  SandboxHealth,
  SandboxMetadata,
} from "./types.js";

/**
 * REST wire client for sandboxd's Filesystem & Runtime API. Owns nothing
 * about port-forwarding or connection lifecycle — callers supply a base URL
 * fixed to one connection generation and an AbortSignal per call.
 * @internal — not part of the public API; see files.ts for the public facade.
 */
export interface SandboxdRestClientOptions {
  /** e.g. "http://127.0.0.1:54321", generation-scoped, never re-resolved. */
  baseUrl: string;
  maxDownloadSize: number;
  maxUploadSize: number;
  maxMetadataResponseSize: number;
}

export type SandboxPathOperation =
  | "read"
  | "write"
  | "exists"
  | "list"
  | "delete";

function invalidArgument(message: string): SandboxError {
  return new SandboxError(message, { telemetryCode: "invalid_argument" });
}

function invalidResponse(message: string, cause?: unknown): SandboxError {
  return new SandboxError(message, {
    telemetryCode: "invalid_response",
    cause,
  });
}

/**
 * Tags an error thrown by a writeStream() caller-supplied source
 * ReadableStream, so Sandbox.classifyOperationFailure() can recognize it and
 * keep it out of connection-generation invalidation regardless of what kind
 * of value it wraps (including `undefined` — hence a class + `instanceof`
 * check rather than an `!== undefined` test), and so the outermost caller
 * (Sandbox.writeStreamImpl) can unwrap it back to the original value before
 * rejecting the public promise. Never exposed to SDK consumers.
 * @internal
 */
export class SourceFailure {
  constructor(readonly value: unknown) {}
}

function hasInvalidChars(p: string): boolean {
  if (p.includes("\0")) return true;
  for (let i = 0; i < p.length; i++) {
    const code = p.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = p.charCodeAt(i + 1);
      if (Number.isNaN(next) || next < 0xdc00 || next > 0xdfff) return true;
      i++;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return true;
    }
  }
  return false;
}

function isRootAlias(p: string): boolean {
  if (p === "") return true;
  return p.split("/").every((seg) => seg === "" || seg === ".");
}

function hasParentSegment(p: string): boolean {
  return p.split("/").some((seg) => seg === "..");
}

/**
 * Validates a caller-supplied sandbox-root-relative path WITHOUT decoding or
 * normalizing it first, then returns the single encodeURIComponent-encoded
 * path segment to append after "/v1/files/" — empty string for the sandbox
 * root. Throws SandboxError before any network request for every case that
 * must be rejected client-side.
 * @internal
 */
export function resolveSandboxPath(
  input: string,
  operation: SandboxPathOperation,
): string {
  if (hasInvalidChars(input)) {
    throw invalidArgument(
      "invalid path: contains a NUL byte or an unpaired UTF-16 surrogate",
    );
  }
  if (input.startsWith("/")) {
    throw invalidArgument(
      `invalid path '${input}': absolute paths are not allowed; paths are relative to the sandbox root`,
    );
  }
  if (hasParentSegment(input)) {
    throw invalidArgument(
      `invalid path '${input}': '..' path segments are not allowed`,
    );
  }
  if (isRootAlias(input)) {
    if (operation === "write" || operation === "delete") {
      throw invalidArgument(
        `invalid path '${input}': the sandbox root cannot be ${
          operation === "write" ? "written to" : "deleted"
        } directly`,
      );
    }
    return "";
  }
  if (
    input.endsWith("/") &&
    (operation === "write" || operation === "delete")
  ) {
    throw invalidArgument(
      `invalid path '${input}': a trailing '/' is not allowed for ${operation}`,
    );
  }
  return encodeURIComponent(input);
}

function mediaType(contentType: string | null): string {
  if (!contentType) return "";
  const idx = contentType.indexOf(";");
  return (idx === -1 ? contentType : contentType.slice(0, idx))
    .trim()
    .toLowerCase();
}

function truncateUtf8(s: string, maxBytes: number): string {
  const bytes = new TextEncoder().encode(s);
  if (bytes.length <= maxBytes) return s;
  return new TextDecoder("utf-8", { fatal: false }).decode(
    bytes.slice(0, maxBytes),
  );
}

/**
 * Converts a body-stream failure (from a Response body reader, or a
 * readStream()/writeStream() wrapper's underlying reader) into the value it
 * should surface as. Shared by readBoundedBody() and the streaming wrappers
 * so both apply the same precedence: a SandboxError we raised ourselves
 * passes through; the caller's own signal (timeout/user-cancel/
 * generation-invalidation) firing mid-stream takes priority over a generic
 * transport failure; anything else is a real transport failure (e.g. the
 * peer reset the connection after sending headers), converted to
 * SandboxConnectionError so Sandbox.classifyOperationFailure() invalidates
 * the shared connection generation instead of leaking a raw TypeError that
 * leaves the generation looking healthy.
 */
function mapBodyStreamError(err: unknown, signal: AbortSignal): unknown {
  if (err instanceof SandboxError) return err;
  if (signal.aborted && signal.reason !== undefined) {
    return signal.reason;
  }
  return new SandboxConnectionError(
    "sandboxd REST response body ended unexpectedly",
    "socket",
    { cause: err, detail: truncateUtf8(String(err), ERROR_DETAIL_MAX_BYTES) },
  );
}

/**
 * Reads a Response body up to maxBytes, cancelling the stream and aborting
 * `controller` on overflow so the underlying socket does not keep streaming
 * data nobody wants. Returns an empty buffer for a null body (HEAD).
 */
async function readBoundedBody(
  response: Response,
  maxBytes: number,
  controller: AbortController,
  signal: AbortSignal,
): Promise<Uint8Array> {
  const reader = response.body?.getReader();
  if (!reader) {
    return new Uint8Array(0);
  }
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maxBytes) {
        controller.abort();
        await reader.cancel().catch(() => {});
        throw new SandboxError(
          "sandboxd response body exceeded the configured size limit",
          { telemetryCode: "response_too_large" },
        );
      }
      chunks.push(value);
    }
  } catch (err) {
    throw mapBodyStreamError(err, signal);
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

/**
 * Wraps a sandboxd response body reader as a pull-based ReadableStream for
 * SandboxdRestClient.readStream(), bounding total bytes by maxDownloadSize
 * and terminating (exactly once — completed/cancelled/failed are mutually
 * exclusive and final) on: normal EOF, consumer cancel() (a normal
 * termination, never converted to an error), overflow, a body error observed
 * either from a pending read() or from `reader.closed` rejecting while the
 * wrapper's single-slot queue is full and no read() is in flight, or `signal`
 * aborting (timeout/user-cancel/lifecycle/generation-invalidation) — checked
 * eagerly, including the case where `signal` is already aborted before this
 * runs, so a caller that never reads and a stalled consumer both still
 * terminate instead of leaking the operation.
 */
function wrapDownloadStream(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  maxBytes: number,
  controller: AbortController,
  signal: AbortSignal,
): ReadableStream<Uint8Array> {
  let total = 0;
  let terminal = false;
  let onAbort: (() => void) | undefined;

  const detach = (): void => {
    if (onAbort) {
      signal.removeEventListener("abort", onAbort);
      onAbort = undefined;
    }
  };

  return new ReadableStream<Uint8Array>(
    {
      start: (rsController) => {
        reader.closed.catch((err: unknown) => {
          if (terminal) return;
          terminal = true;
          detach();
          rsController.error(mapBodyStreamError(err, signal));
        });
        const handleAbort = (): void => {
          if (terminal) return;
          terminal = true;
          detach();
          reader.cancel().catch(() => {});
          rsController.error(signal.reason);
        };
        if (signal.aborted) {
          handleAbort();
          return;
        }
        onAbort = handleAbort;
        signal.addEventListener("abort", onAbort, { once: true });
      },
      pull: async (rsController) => {
        if (terminal) return;
        let result: ReadableStreamReadResult<Uint8Array>;
        try {
          result = await reader.read();
        } catch (err) {
          if (terminal) return;
          terminal = true;
          detach();
          rsController.error(mapBodyStreamError(err, signal));
          return;
        }
        if (terminal) return;
        if (result.done) {
          terminal = true;
          detach();
          rsController.close();
          return;
        }
        total += result.value.byteLength;
        if (total > maxBytes) {
          terminal = true;
          detach();
          const err = new SandboxError(
            "sandboxd response body exceeded the configured size limit",
            { telemetryCode: "response_too_large" },
          );
          controller.abort(err);
          reader.cancel().catch(() => {});
          rsController.error(err);
          return;
        }
        rsController.enqueue(result.value);
      },
      cancel: (reason) => {
        if (terminal) return;
        terminal = true;
        detach();
        controller.abort(reason);
        reader.cancel(reason).catch(() => {});
      },
    },
    { highWaterMark: 1 },
  );
}

/**
 * Wraps a caller-supplied writeStream() source reader as a pull-based
 * ReadableStream to hand to fetch() as the PUT body, bounding total bytes by
 * maxUploadSize (aborting `controller` before an over-limit chunk is ever
 * enqueued — the server never receives more than the limit) and tagging any
 * error the source itself throws (from a pending read() or from
 * `sourceReader.closed` rejecting) as SourceFailure so it survives
 * classifyOperationFailure() untouched. Does not cancel `sourceReader` on its
 * own cancel() — SandboxdRestClient.writeStream() owns that centrally so
 * every termination path (overflow, source failure, external abort, early
 * HTTP response) cancels the source exactly once, in one place.
 */
function wrapUploadStream(
  sourceReader: ReadableStreamDefaultReader<Uint8Array>,
  maxBytes: number,
  controller: AbortController,
): {
  stream: ReadableStream<Uint8Array>;
  bytesSent: () => number;
  isSourceDone: () => boolean;
} {
  let total = 0;
  let done = false;
  let terminal = false;

  const stream = new ReadableStream<Uint8Array>(
    {
      start: (rsController) => {
        sourceReader.closed.catch((err: unknown) => {
          if (terminal) return;
          terminal = true;
          const tagged = new SourceFailure(err);
          controller.abort(tagged);
          rsController.error(tagged);
        });
      },
      pull: async (rsController) => {
        if (terminal) return;
        let result: ReadableStreamReadResult<Uint8Array>;
        try {
          result = await sourceReader.read();
        } catch (err) {
          if (terminal) return;
          terminal = true;
          const tagged = new SourceFailure(err);
          controller.abort(tagged);
          rsController.error(tagged);
          return;
        }
        if (terminal) return;
        if (result.done) {
          terminal = true;
          done = true;
          rsController.close();
          return;
        }
        total += result.value.byteLength;
        if (total > maxBytes) {
          terminal = true;
          const err = new SandboxError(
            `content exceeds the configured upload limit of ${maxBytes} bytes`,
            { telemetryCode: "request_too_large" },
          );
          controller.abort(err);
          rsController.error(err);
          return;
        }
        rsController.enqueue(result.value);
      },
      cancel: () => {
        terminal = true;
      },
    },
    { highWaterMark: 1 },
  );
  return { stream, bytesSent: () => total, isSourceDone: () => done };
}

const KNOWN_API_CODES: ReadonlySet<string> = new Set([
  "NOT_FOUND",
  "PERMISSION_DENIED",
  "CONFLICT",
  "INTERNAL",
]);

function normalizeApiCode(
  status: number,
  serverCode?: string,
): SandboxdApiCode {
  if (serverCode && KNOWN_API_CODES.has(serverCode)) {
    return serverCode as SandboxdApiCode;
  }
  switch (status) {
    case 404:
      return "NOT_FOUND";
    case 403:
      return "PERMISSION_DENIED";
    case 409:
      return "CONFLICT";
    case 500:
      return "INTERNAL";
    default:
      return "UNKNOWN";
  }
}

export class SandboxdRestClient {
  constructor(private readonly opts: SandboxdRestClientOptions) {}

  private url(path: string, query?: Record<string, string>): string {
    const qs = query ? `?${new URLSearchParams(query).toString()}` : "";
    return `${this.opts.baseUrl}${path}${qs}`;
  }

  /**
   * Performs one request and returns the raw fetch Response plus a
   * controller the caller can use to abort mid-body-read. Every request path
   * (health, success, failure) shares this: `redirect: "error"` and a signal.
   */
  private async request(
    method: string,
    path: string,
    init: {
      query?: Record<string, string>;
      body?: BodyInit;
      headers?: Record<string, string>;
      signal: AbortSignal;
      /**
       * Caller-owned controller to abort mid-body-read/write (overflow,
       * source failure). Defaults to a fresh one. writeStream() passes its
       * own so it can abort the request from inside the upload wrapper's
       * pull(), before any response has arrived.
       */
      controller?: AbortController;
      /** Required by fetch() when `body` is a ReadableStream. */
      duplex?: "half";
    },
  ): Promise<{ response: Response; controller: AbortController }> {
    // `controller` exists only so readBoundedBody()/the streaming wrappers
    // can abort on overflow or source failure; `combined` is what actually
    // goes to fetch(), and it stays linked to `init.signal` for the
    // request's whole lifetime — including body streaming, which happens
    // well after this method returns. Detaching from init.signal once
    // headers arrive (as an addEventListener-based bridge into `controller`
    // alone would) would let a timeout/user-abort during body streaming go
    // unnoticed: reader.read() would just hang.
    const controller = init.controller ?? new AbortController();
    const combined = AbortSignal.any([init.signal, controller.signal]);
    // `duplex` isn't in lib.dom's RequestInit yet, though it's required by
    // the Fetch spec (and undici) whenever `body` is a ReadableStream.
    const fetchInit: RequestInit & { duplex?: "half" } = {
      method,
      body: init.body,
      headers: init.headers,
      redirect: "error",
      signal: combined,
    };
    if (init.duplex) {
      fetchInit.duplex = init.duplex;
    }
    let response: Response;
    try {
      response = await fetch(this.url(path, init.query), fetchInit);
    } catch (err) {
      if (combined.aborted && combined.reason !== undefined) {
        throw combined.reason;
      }
      throw new SandboxConnectionError(
        `sandboxd REST request failed: ${method} ${path}`,
        "socket",
        {
          cause: err,
          detail: truncateUtf8(String(err), ERROR_DETAIL_MAX_BYTES),
        },
      );
    }
    return { response, controller };
  }

  private async buildApiError(
    response: Response,
    controller: AbortController,
    method: string,
    path: string,
    signal: AbortSignal,
  ): Promise<SandboxdApiError> {
    let bodyBytes: Uint8Array;
    try {
      bodyBytes = await readBoundedBody(
        response,
        this.opts.maxMetadataResponseSize,
        controller,
        signal,
      );
    } catch {
      return new SandboxdApiError(
        `sandboxd ${method} ${path} failed with HTTP ${response.status}`,
        response.status,
        normalizeApiCode(response.status),
      );
    }
    const text = new TextDecoder().decode(bodyBytes);
    let serverCode: string | undefined;
    let serverMessage: string | undefined;
    if (text.length > 0) {
      try {
        const parsed: unknown = JSON.parse(text);
        if (parsed && typeof parsed === "object") {
          const obj = parsed as Record<string, unknown>;
          if (typeof obj.code === "string") serverCode = obj.code;
          if (typeof obj.message === "string") serverMessage = obj.message;
        }
      } catch {
        // Non-JSON or empty error body: fall through with the raw text preview.
      }
    }
    const preview = truncateUtf8(serverMessage ?? text, ERROR_DETAIL_MAX_BYTES);
    return new SandboxdApiError(
      `sandboxd ${method} ${path} failed with HTTP ${response.status}`,
      response.status,
      normalizeApiCode(response.status, serverCode),
      preview.length > 0 ? { detail: preview } : undefined,
    );
  }

  async health(signal: AbortSignal): Promise<SandboxHealth> {
    const { response, controller } = await this.request("GET", "/v1/health", {
      signal,
    });
    if (response.status !== 200) {
      throw await this.buildApiError(
        response,
        controller,
        "GET",
        "/v1/health",
        signal,
      );
    }
    const bodyBytes = await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    );
    let parsed: unknown;
    try {
      parsed = JSON.parse(new TextDecoder().decode(bodyBytes));
    } catch (err) {
      throw invalidResponse("sandboxd /v1/health returned malformed JSON", err);
    }
    if (typeof parsed !== "object" || parsed === null) {
      throw invalidResponse("sandboxd /v1/health returned a non-object body");
    }
    const obj = parsed as Record<string, unknown>;
    if (
      obj.status !== "ok" ||
      typeof obj.uptime_seconds !== "number" ||
      !Number.isSafeInteger(obj.uptime_seconds) ||
      obj.uptime_seconds < 0
    ) {
      throw invalidResponse(
        "sandboxd /v1/health returned an unexpected body shape",
      );
    }
    return { status: obj.status, uptimeSeconds: obj.uptime_seconds };
  }

  async metadata(signal: AbortSignal): Promise<SandboxMetadata> {
    const { response, controller } = await this.request("GET", "/v1/metadata", {
      signal,
    });
    if (response.status !== 200) {
      throw await this.buildApiError(
        response,
        controller,
        "GET",
        "/v1/metadata",
        signal,
      );
    }
    const bodyBytes = await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    );
    let parsed: unknown;
    try {
      parsed = JSON.parse(new TextDecoder().decode(bodyBytes));
    } catch (err) {
      throw invalidResponse(
        "sandboxd /v1/metadata returned malformed JSON",
        err,
      );
    }
    return parseMetadata(parsed);
  }

  async read(path: string, signal: AbortSignal): Promise<Uint8Array> {
    const encoded = resolveSandboxPath(path, "read");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("GET", reqPath, {
      signal,
    });
    if (response.status !== 200) {
      throw await this.buildApiError(
        response,
        controller,
        "GET",
        reqPath,
        signal,
      );
    }
    if (
      mediaType(response.headers.get("content-type")) === "application/json"
    ) {
      await readBoundedBody(
        response,
        this.opts.maxMetadataResponseSize,
        controller,
        signal,
      ).catch(() => {});
      throw invalidArgument(`cannot read '${path}': it is a directory`);
    }
    return readBoundedBody(
      response,
      this.opts.maxDownloadSize,
      controller,
      signal,
    );
  }

  /**
   * Like read(), but resolves once headers are validated (200, non-JSON)
   * instead of buffering the whole body — see wrapDownloadStream() for the
   * streaming/termination contract.
   */
  async readStream(
    path: string,
    signal: AbortSignal,
  ): Promise<ReadableStream<Uint8Array>> {
    const encoded = resolveSandboxPath(path, "read");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("GET", reqPath, {
      signal,
    });
    if (response.status !== 200) {
      throw await this.buildApiError(
        response,
        controller,
        "GET",
        reqPath,
        signal,
      );
    }
    if (
      mediaType(response.headers.get("content-type")) === "application/json"
    ) {
      await readBoundedBody(
        response,
        this.opts.maxMetadataResponseSize,
        controller,
        signal,
      ).catch(() => {});
      throw invalidArgument(`cannot read '${path}': it is a directory`);
    }
    const reader = response.body?.getReader();
    if (!reader) {
      return new ReadableStream<Uint8Array>({
        start: (rsController) => rsController.close(),
      });
    }
    return wrapDownloadStream(
      reader,
      this.opts.maxDownloadSize,
      controller,
      signal,
    );
  }

  async write(
    path: string,
    content: Uint8Array,
    opts: { mode?: string },
    signal: AbortSignal,
  ): Promise<void> {
    if (opts.mode !== undefined && !/^0[0-7]{3}$/.test(opts.mode)) {
      throw invalidArgument(
        `invalid mode '${opts.mode}': must match ^0[0-7]{3}$`,
      );
    }
    if (content.byteLength > this.opts.maxUploadSize) {
      throw invalidArgument(
        `content of ${content.byteLength} bytes exceeds the configured upload limit of ${this.opts.maxUploadSize} bytes`,
      );
    }
    const encoded = resolveSandboxPath(path, "write");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("PUT", reqPath, {
      signal,
      query: opts.mode ? { mode: opts.mode } : undefined,
      headers: { "Content-Type": "application/octet-stream" },
      body: content as BodyInit,
    });
    if (response.status !== 204) {
      throw await this.buildApiError(
        response,
        controller,
        "PUT",
        reqPath,
        signal,
      );
    }
    await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    ).catch(() => {});
  }

  /**
   * Like write(), but streams `content` in a single chunked request instead
   * of buffering it, bounded by maxUploadSize enforced per-chunk (see
   * wrapUploadStream()). Returns the number of bytes actually sent, for the
   * caller's tracing span. `content` is consumed at most once; a source
   * error surfaces tagged as SourceFailure (see that class's doc) rather
   * than thrown directly, so Sandbox can keep it out of connection-
   * generation invalidation and unwrap it for the caller.
   *
   * Every termination path (overflow, source failure, external `signal`
   * abort, a non-204 response, or an acknowledgement before the source
   * reached EOF) cancels `content`'s reader exactly once, centrally, here —
   * fetch()'s own cancellation of the request body on an early response is
   * asynchronous and unreliable (observed: it can lag the resolved Response
   * by tens of milliseconds, or never fire on an external abort at all), so
   * this method never relies on it.
   */
  async writeStream(
    path: string,
    content: ReadableStream<Uint8Array>,
    opts: { mode?: string },
    signal: AbortSignal,
  ): Promise<number> {
    if (opts.mode !== undefined && !/^0[0-7]{3}$/.test(opts.mode)) {
      throw invalidArgument(
        `invalid mode '${opts.mode}': must match ^0[0-7]{3}$`,
      );
    }
    if (content.locked) {
      throw invalidArgument(
        "writeStream content is already locked by another reader",
      );
    }
    const encoded = resolveSandboxPath(path, "write");
    const reqPath = `/v1/files/${encoded}`;

    let sourceReader: ReadableStreamDefaultReader<Uint8Array>;
    try {
      sourceReader = content.getReader();
    } catch {
      throw invalidArgument(
        "writeStream content is already locked by another reader",
      );
    }

    const controller = new AbortController();
    const {
      stream: body,
      bytesSent,
      isSourceDone,
    } = wrapUploadStream(sourceReader, this.opts.maxUploadSize, controller);

    let response: Response;
    try {
      ({ response } = await this.request("PUT", reqPath, {
        signal,
        controller,
        duplex: "half",
        query: opts.mode ? { mode: opts.mode } : undefined,
        headers: { "Content-Type": "application/octet-stream" },
        body: body as BodyInit,
      }));
    } catch (err) {
      sourceReader.cancel(err).catch(() => {});
      throw err;
    }

    if (response.status !== 204) {
      sourceReader.cancel().catch(() => {});
      throw await this.buildApiError(
        response,
        controller,
        "PUT",
        reqPath,
        signal,
      );
    }
    if (!isSourceDone()) {
      sourceReader.cancel().catch(() => {});
      await readBoundedBody(
        response,
        this.opts.maxMetadataResponseSize,
        controller,
        signal,
      ).catch(() => {});
      throw invalidResponse(
        "sandboxd acknowledged the write before the input stream reached EOF",
      );
    }
    await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    ).catch(() => {});
    return bytesSent();
  }

  async exists(path: string, signal: AbortSignal): Promise<boolean> {
    const encoded = resolveSandboxPath(path, "exists");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("HEAD", reqPath, {
      signal,
    });
    if (response.status === 404) return false;
    if (response.status >= 200 && response.status < 300) return true;
    throw await this.buildApiError(
      response,
      controller,
      "HEAD",
      reqPath,
      signal,
    );
  }

  async list(path: string, signal: AbortSignal): Promise<DirectoryListing> {
    const encoded = resolveSandboxPath(path, "list");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("GET", reqPath, {
      signal,
    });
    if (response.status !== 200) {
      throw await this.buildApiError(
        response,
        controller,
        "GET",
        reqPath,
        signal,
      );
    }
    if (
      mediaType(response.headers.get("content-type")) !== "application/json"
    ) {
      await readBoundedBody(
        response,
        this.opts.maxDownloadSize,
        controller,
        signal,
      ).catch(() => {});
      throw invalidArgument(
        `cannot list '${path}': it is a file, not a directory`,
      );
    }
    const bodyBytes = await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    );
    let parsed: unknown;
    try {
      parsed = JSON.parse(new TextDecoder().decode(bodyBytes));
    } catch (err) {
      throw invalidResponse(
        "sandboxd returned malformed directory listing JSON",
        err,
      );
    }
    return parseDirectoryListing(parsed);
  }

  async delete(
    path: string,
    opts: { recursive?: boolean },
    signal: AbortSignal,
  ): Promise<void> {
    const encoded = resolveSandboxPath(path, "delete");
    const reqPath = `/v1/files/${encoded}`;
    const { response, controller } = await this.request("DELETE", reqPath, {
      signal,
      query: opts.recursive ? { recursive: "true" } : undefined,
    });
    if (response.status !== 204) {
      throw await this.buildApiError(
        response,
        controller,
        "DELETE",
        reqPath,
        signal,
      );
    }
    await readBoundedBody(
      response,
      this.opts.maxMetadataResponseSize,
      controller,
      signal,
    ).catch(() => {});
  }
}

const RFC3339_RE =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;

function isLeapYear(year: number): boolean {
  return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0;
}

function isValidRfc3339(s: string): boolean {
  const m = RFC3339_RE.exec(s);
  if (!m) return false;
  const year = Number(m[1]);
  const month = Number(m[2]);
  const day = Number(m[3]);
  const hour = Number(m[4]);
  const minute = Number(m[5]);
  const second = Number(m[6]);
  const offset = m[7];
  if (month < 1 || month > 12) return false;
  const daysInMonth = [
    31,
    isLeapYear(year) ? 29 : 28,
    31,
    30,
    31,
    30,
    31,
    31,
    30,
    31,
    30,
    31,
  ];
  if (day < 1 || day > daysInMonth[month - 1]) return false;
  if (hour > 23 || minute > 59 || second > 60) return false;
  if (offset.toUpperCase() !== "Z") {
    const om = /^([+-])(\d{2}):(\d{2})$/.exec(offset);
    if (!om) return false;
    if (Number(om[2]) > 23 || Number(om[3]) > 59) return false;
  }
  return true;
}

const MODE_RE = /^0[0-7]{3,4}$/;

function parseMetadata(raw: unknown): SandboxMetadata {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw invalidResponse("sandboxd /v1/metadata returned a non-object body");
  }
  const rawEnv = (raw as Record<string, unknown>).env;
  if (rawEnv === undefined) {
    return { env: {} };
  }
  if (typeof rawEnv !== "object" || rawEnv === null || Array.isArray(rawEnv)) {
    throw invalidResponse("sandboxd /v1/metadata has a non-object 'env'");
  }
  // defineProperty below, not assignment: env names come from the server, and
  // assigning a "__proto__" key on an ordinary object would rewrite its
  // prototype instead of creating an entry.
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(rawEnv)) {
    if (typeof value !== "string") {
      // The name is left out of the message: it is server-controlled data.
      throw invalidResponse(
        "sandboxd /v1/metadata has a non-string 'env' value",
      );
    }
    Object.defineProperty(env, name, {
      value,
      enumerable: true,
      writable: true,
      configurable: true,
    });
  }
  return { env };
}

function parseDirectoryListing(raw: unknown): DirectoryListing {
  if (typeof raw !== "object" || raw === null) {
    throw invalidResponse("sandboxd returned a non-object directory listing");
  }
  const obj = raw as Record<string, unknown>;
  if (typeof obj.path !== "string") {
    throw invalidResponse("sandboxd directory listing is missing 'path'");
  }
  if (!Array.isArray(obj.entries)) {
    throw invalidResponse("sandboxd directory listing is missing 'entries'");
  }
  const entries: FileEntry[] = [];
  for (const rawEntry of obj.entries) {
    if (typeof rawEntry !== "object" || rawEntry === null) {
      throw invalidResponse(
        "sandboxd directory listing contains a non-object entry",
      );
    }
    const e = rawEntry as Record<string, unknown>;
    if (typeof e.type !== "string") {
      throw invalidResponse("sandboxd directory entry has a non-string 'type'");
    }
    if (e.type !== "file" && e.type !== "directory") {
      // Unknown entry type: skip, without logging the entry's name or type.
      continue;
    }
    if (typeof e.name !== "string" || e.name.length === 0) {
      throw invalidResponse("sandboxd directory entry has an invalid 'name'");
    }
    if (
      typeof e.size !== "number" ||
      !Number.isSafeInteger(e.size) ||
      e.size < 0
    ) {
      throw invalidResponse("sandboxd directory entry has an invalid 'size'");
    }
    if (typeof e.modified_at !== "string" || !isValidRfc3339(e.modified_at)) {
      throw invalidResponse(
        "sandboxd directory entry has an invalid 'modified_at'",
      );
    }
    let mode: string | undefined;
    if (e.mode !== undefined) {
      if (typeof e.mode !== "string" || !MODE_RE.test(e.mode)) {
        throw invalidResponse("sandboxd directory entry has an invalid 'mode'");
      }
      mode = e.mode;
    }
    entries.push({
      name: e.name,
      size: e.size,
      type: e.type,
      modifiedAt: e.modified_at,
      mode,
    });
  }
  return { path: obj.path, entries };
}
