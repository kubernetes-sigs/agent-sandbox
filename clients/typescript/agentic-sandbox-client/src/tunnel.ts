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

import * as net from "node:net";
import { Readable, Writable } from "node:stream";
import type * as k8s from "@kubernetes/client-node";
// Not re-exported from the package root (see index.d.ts's export list), so
// this reaches into the compiled output directly. @kubernetes/client-node
// declares no "exports" map, so Node's default resolution allows it.
import { WebSocketHandler } from "@kubernetes/client-node/dist/web-socket-handler.js";
import WebSocket from "ws";
import {
  ERROR_DETAIL_MAX_BYTES,
  MAX_ERROR_CHANNEL_PAYLOAD_BYTES,
  PORT_FORWARD_DATA_CHANNEL,
  PORT_FORWARD_ERROR_CHANNEL,
  TUNNEL_BACKPRESSURE_THRESHOLD_BYTES,
} from "./constants.js";
import { SandboxConnectionError, SandboxTimeoutError } from "./exceptions.js";
import type { Logger } from "./types.js";

/**
 * PodTunnel owns exactly two local TCP listeners (REST, gRPC) that bridge to
 * the same Pod's sandboxd ports over Kubernetes' WebSocket port-forward
 * sub-protocol — one WebSocket per accepted TCP connection. It knows nothing
 * about REST/gRPC semantics or claim lifecycle; see rest.ts/process.ts for
 * the protocols carried over these tunnels and sandbox.ts for lifecycle.
 * @internal — not part of the public API.
 */
export interface PodTunnelOptions {
  kubeConfig: k8s.KubeConfig;
  namespace: string;
  podName: string;
  restTargetPort: number;
  grpcTargetPort: number;
  /** Budget for each individual WS handshake (one per accepted TCP socket). */
  handshakeTimeoutMs: number;
  logger: Logger;
  /**
   * Test seam: overrides the WebSocket implementation used to dial the
   * apiserver. Defaults to the `ws` package, matching production.
   */
  webSocketFactory?: (
    uri: string,
    protocols: string[],
    opts: WebSocket.ClientOptions,
  ) => WebSocket;
}

export interface PodTunnelEndpoints {
  restBaseUrl: string;
  grpcBaseUrl: string;
}

function truncateUtf8(s: string, maxBytes: number): string {
  const bytes = new TextEncoder().encode(s);
  if (bytes.length <= maxBytes) return s;
  return new TextDecoder("utf-8", { fatal: false }).decode(
    bytes.slice(0, maxBytes),
  );
}

/**
 * Accumulates the first 2 bytes of a port-forward channel's byte stream (the
 * port-number header k8s prepends to the first frame of each channel),
 * across as many messages as it takes to see both bytes, and returns
 * whatever of a given chunk lies past the header.
 */
class ChannelHeader {
  private len = 0;

  consume(data: Buffer): Buffer {
    if (this.len >= 2) return data;
    const need = 2 - this.len;
    const take = Math.min(need, data.length);
    this.len += take;
    return data.subarray(take);
  }
}

/**
 * Minimal shape PodTunnel needs from a "WebSocket-handler-compatible" object:
 * exactly what @kubernetes/client-node's WebSocketHandler.connect() reads or
 * assigns on the object returned by the injected socketFactory. Kept
 * separate from the real `ws.WebSocket` type because PodTunnel intentionally
 * never lets WebSocketHandler's own onmessage run (see wrapForHandler below).
 */
interface HandlerFacade {
  onopen: (() => void) | null;
  onerror: ((err: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  readyState: number;
  protocol: string;
  close(code?: number, reason?: string): void;
}

interface PairHandle {
  teardown(reason?: unknown): void;
  teardownGraceful(): void;
  /** True once either teardown method has begun (forceful or graceful). */
  readonly settling: boolean;
}

/**
 * PodTunnel bridges two local TCP listeners to a Pod's REST and gRPC ports
 * via Kubernetes' WebSocket port-forward sub-protocol.
 *
 * Local listeners have no authentication of their own: any other process in
 * the same network namespace can reach the sandbox's files/run API through
 * them for as long as the tunnel is open. See the SDK README's trust-model
 * note.
 */
export class PodTunnel {
  private restServer: net.Server | null = null;
  private grpcServer: net.Server | null = null;
  private restLocalPort = 0;
  private grpcLocalPort = 0;
  private closed = false;

  private readonly pendingSockets = new Set<net.Socket>();
  private readonly pendingWebSockets = new Set<WebSocket>();
  private readonly pairs = new Set<PairHandle>();

  /**
   * The most recent WS handshake failure, kept so a caller polling this
   * tunnel (e.g. a REST health-check retry loop) can distinguish a
   * permanent rejection — auth denied, Pod not found — from a transient one
   * and fail fast instead of retrying for the full connect budget.
   */
  private _lastHandshakeError: unknown = null;

  constructor(private readonly opts: PodTunnelOptions) {}

  get endpoints(): PodTunnelEndpoints {
    return {
      restBaseUrl: `http://127.0.0.1:${this.restLocalPort}`,
      grpcBaseUrl: `http://127.0.0.1:${this.grpcLocalPort}`,
    };
  }

  /**
   * Returns the most recent WS handshake failure seen by this tunnel, or
   * `null` if every handshake so far has succeeded. `ws` reports a rejected
   * HTTP upgrade (401/403/404 from the apiserver) as an Error whose message
   * contains "Unexpected server response: <status>" — see
   * isTerminalPortForwardFailure() below.
   */
  get lastHandshakeError(): unknown {
    return this._lastHandshakeError;
  }

  /**
   * Opens both local listeners. On failure, any listener already opened is
   * closed before the error is thrown so no listener leaks past a partial
   * failure.
   */
  async start(): Promise<PodTunnelEndpoints> {
    this.restServer = net.createServer((socket) =>
      this.handleAccept(socket, this.opts.restTargetPort),
    );
    try {
      this.restLocalPort = await this.listen(this.restServer);
    } catch (err) {
      this.restServer = null;
      throw new SandboxConnectionError(
        "failed to open the local REST port-forward listener",
        "listener",
        { cause: err },
      );
    }

    this.grpcServer = net.createServer((socket) =>
      this.handleAccept(socket, this.opts.grpcTargetPort),
    );
    try {
      this.grpcLocalPort = await this.listen(this.grpcServer);
    } catch (err) {
      await this.closeServer(this.restServer);
      this.restServer = null;
      this.grpcServer = null;
      throw new SandboxConnectionError(
        "failed to open the local gRPC port-forward listener",
        "listener",
        { cause: err },
      );
    }

    return this.endpoints;
  }

  private listen(server: net.Server): Promise<number> {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => {
        server.off("error", reject);
        const addr = server.address();
        if (!addr || typeof addr === "string") {
          reject(new Error("failed to bind local port-forward listener"));
          return;
        }
        resolve(addr.port);
      });
    });
  }

  private closeServer(server: net.Server | null): Promise<void> {
    if (!server) return Promise.resolve();
    return new Promise((resolve) => server.close(() => resolve()));
  }

  /** Closes both listeners and terminates every socket/WS this tunnel owns. */
  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;

    for (const socket of this.pendingSockets) socket.destroy();
    this.pendingSockets.clear();
    for (const ws of this.pendingWebSockets) ws.terminate();
    this.pendingWebSockets.clear();
    for (const pair of this.pairs) pair.teardown();
    this.pairs.clear();

    await Promise.all([
      this.closeServer(this.restServer),
      this.closeServer(this.grpcServer),
    ]);
  }

  private portForwardPath(targetPort: number): string {
    const ns = encodeURIComponent(this.opts.namespace);
    const pod = encodeURIComponent(this.opts.podName);
    return `/api/v1/namespaces/${ns}/pods/${pod}/portforward?ports=${targetPort}`;
  }

  private handleAccept(socket: net.Socket, targetPort: number): void {
    if (this.closed) {
      socket.destroy();
      return;
    }
    // Paused immediately; only resumed once the paired WS is open and the
    // pump is wired, so no data can be lost or misrouted before pairing.
    socket.pause();
    socket.on("error", () => {});
    this.pendingSockets.add(socket);
    this.connectOne(socket, targetPort)
      .catch(() => {})
      .finally(() => {
        this.pendingSockets.delete(socket);
      });
  }

  private async connectOne(
    socket: net.Socket,
    targetPort: number,
  ): Promise<void> {
    const controller = new AbortController();
    const timer = setTimeout(() => {
      controller.abort(
        new SandboxTimeoutError(
          `port-forward WebSocket handshake did not complete within ${this.opts.handshakeTimeoutMs}ms`,
        ),
      );
    }, this.opts.handshakeTimeoutMs);
    const onSocketDown = () =>
      controller.abort(
        new SandboxConnectionError(
          "local TCP connection closed before the port-forward handshake completed",
          "socket",
        ),
      );
    socket.once("close", onSocketDown);
    socket.once("error", onSocketDown);

    let real: WebSocket | undefined;
    let pairHandle: PairHandle | undefined;
    const noopStream = {
      stdin: new Readable({ read() {} }),
      stdout: new Writable({
        write(_c, _e, cb) {
          cb();
        },
      }),
      stderr: new Writable({
        write(_c, _e, cb) {
          cb();
        },
      }),
    };
    const factory = (
      uri: string,
      protocols: string[],
      wsOpts: WebSocket.ClientOptions,
    ): WebSocket => {
      if (this.closed || controller.signal.aborted || socket.destroyed) {
        return this.inertSocket();
      }
      const created = this.opts.webSocketFactory
        ? this.opts.webSocketFactory(uri, protocols, wsOpts)
        : new WebSocket(uri, protocols, {
            ...wsOpts,
            handshakeTimeout: this.opts.handshakeTimeoutMs,
          });
      // Permanent safety net: Node throws on an unhandled EventEmitter
      // "error" event. The handshake-scoped listener below is `once` and
      // self-removes; this keeps the emitter safe for the object's whole
      // life regardless of when a later error arrives.
      created.on("error", () => {});
      real = created;
      this.pendingWebSockets.add(created);
      if (this.closed || controller.signal.aborted || socket.destroyed) {
        created.terminate();
        this.pendingWebSockets.delete(created);
        return this.inertSocket();
      }
      // Wired immediately, not after the handshake settles: the apiserver's
      // per-channel port-number header (and any real data) can arrive before
      // handler.connect()'s promise resolves, and a "message" event with no
      // listener attached is lost forever, not queued.
      pairHandle = this.wireMessagePump(socket, created);
      return this.wrapForHandler(created, controller);
    };

    const handler = new WebSocketHandler(
      this.opts.kubeConfig,
      factory as unknown as ConstructorParameters<typeof WebSocketHandler>[1],
      noopStream,
    );

    try {
      await Promise.race([
        handler.connect(this.portForwardPath(targetPort), null, null),
        this.rejectOnAbort(controller.signal),
      ]);
    } catch (err) {
      this._lastHandshakeError = err;
      if (real) this.pendingWebSockets.delete(real);
      if (pairHandle) {
        pairHandle.teardown(err);
      } else {
        real?.terminate();
        socket.destroy();
      }
      this.opts.logger.debug(
        `sandboxd port-forward handshake failed: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    } finally {
      clearTimeout(timer);
      socket.off("close", onSocketDown);
      socket.off("error", onSocketDown);
    }

    if (
      !real ||
      !pairHandle ||
      this.closed ||
      controller.signal.aborted ||
      socket.destroyed
    ) {
      if (real) this.pendingWebSockets.delete(real);
      if (pairHandle) {
        pairHandle.teardown();
      } else {
        real?.terminate();
        socket.destroy();
      }
      return;
    }

    this.pendingWebSockets.delete(real);
    this.pendingSockets.delete(socket);
    this.wireSocketToWs(socket, real, pairHandle);
    socket.resume();
  }

  private rejectOnAbort(signal: AbortSignal): Promise<never> {
    return new Promise((_resolve, reject) => {
      if (signal.aborted) {
        reject(signal.reason);
        return;
      }
      signal.addEventListener("abort", () => reject(signal.reason), {
        once: true,
      });
    });
  }

  private inertSocket(): WebSocket {
    // Satisfies WebSocketHandler.connect()'s factory contract without
    // starting a network connection for an attempt that is already doomed
    // (tunnel closed / deadline expired / TCP peer gone). connect()'s
    // promise simply never settles from this object; the caller is always
    // racing it against its own deadline/abort rejection.
    return {
      onopen: null,
      onerror: null,
      onmessage: null,
      readyState: WebSocket.CLOSED,
      protocol: "",
      close: () => {},
    } as unknown as WebSocket;
  }

  /**
   * Wraps the real `ws` socket in a plain object satisfying
   * WebSocketHandler's expectations (onopen/onerror assigned as plain
   * properties, not addEventListener). `onmessage` is deliberately never
   * invoked: WebSocketHandler's own onmessage reads bytes unconditionally
   * (crashing on a short frame) and calls closeStream() against a
   * stdin/stdout/stderr triple that has nothing to do with this tunnel.
   * PodTunnel's pump (installed in wireMessagePump) listens on the real socket
   * directly instead.
   */
  private wrapForHandler(
    real: WebSocket,
    controller: AbortController,
  ): WebSocket {
    const facade: HandlerFacade = {
      onopen: null,
      onerror: null,
      onmessage: null,
      readyState: real.readyState,
      protocol: real.protocol,
      close: (code, reason) => real.close(code, reason),
    };
    let settled = false;
    const onOpen = () => {
      if (settled) return;
      settled = true;
      facade.readyState = real.readyState;
      facade.protocol = real.protocol;
      facade.onopen?.();
    };
    // A close with no preceding "error" (e.g. the apiserver rejects the
    // upgrade cleanly, or Pod 404) would otherwise leave connect()'s promise
    // pending forever, since it only resolves on open and rejects on error.
    const onCloseOrError = (err?: unknown) => {
      if (settled) return;
      settled = true;
      facade.onerror?.(
        err ?? new Error("port-forward WebSocket closed before opening"),
      );
    };
    real.once("open", onOpen);
    real.once("error", onCloseOrError);
    real.once("close", () => onCloseOrError());
    controller.signal.addEventListener(
      "abort",
      () => {
        real.off("open", onOpen);
        real.off("error", onCloseOrError);
      },
      { once: true },
    );
    return facade as unknown as WebSocket;
  }

  /**
   * Wires the WS->TCP direction and the pair's teardown handle, immediately
   * upon WS creation — before the handshake even settles. The apiserver's
   * per-channel port-number header (and, in principle, real data) can arrive
   * before handler.connect()'s promise resolves; an EventEmitter "message"
   * event with no listener attached is dropped, not queued, so waiting until
   * pairing is confirmed would silently lose those bytes. Writing to `socket`
   * before wireSocketToWs()/resume() is safe: `pause()`/`resume()` only gate
   * the readable side emitting "data", never the writable side.
   */
  private wireMessagePump(socket: net.Socket, real: WebSocket): PairHandle {
    // "graceful" covers the WS or the local socket closing cleanly on its
    // own — draining flushes whatever is already queued instead of
    // discarding it. Forceful teardown (errors, protocol violations,
    // PodTunnel.close()) always wins and can cut in mid-drain, which is why
    // it only guards on "closed", not "graceful": close() depends on being
    // able to force every remaining pair so it doesn't hang waiting for a
    // slow reader to finish draining.
    let state: "open" | "graceful" | "closed" = "open";

    const finalize = () => {
      if (state === "closed") return;
      state = "closed";
      this.pairs.delete(pairHandle);
    };

    const forceTeardown = () => {
      if (state === "closed") return;
      finalize();
      socket.destroy();
      real.terminate();
    };

    const gracefulTeardown = () => {
      if (state !== "open") return;
      state = "graceful";
      real.close();
      if (socket.destroyed) {
        finalize();
      } else {
        // end() flushes whatever the "message" handler below already wrote
        // before closing, unlike destroy() which discards it. finalize()
        // (and the this.pairs removal close() depends on) waits for the
        // socket to actually finish closing.
        socket.end();
        socket.once("close", finalize);
      }
    };

    const pairHandle: PairHandle = {
      teardown: forceTeardown,
      teardownGraceful: gracefulTeardown,
      get settling() {
        return state !== "open";
      },
    };
    this.pairs.add(pairHandle);

    const dataHeader = new ChannelHeader();
    const errorHeader = new ChannelHeader();
    const errorChunks: Buffer[] = [];

    // Registered here, not in wireSocketToWs, because the message handler
    // below can call real.pause() (backpressure) as soon as messages start
    // arriving — before the handshake even settles — and pause() is only
    // ever lifted by this "drain" listener.
    socket.on("drain", () => real.resume());

    real.on("close", () => pairHandle.teardownGraceful());
    real.on("error", () => pairHandle.teardown());
    real.on("message", (raw: Buffer, isBinary: boolean) => {
      if (pairHandle.settling) return;
      if (!isBinary || !(raw instanceof Buffer) || raw.length === 0) {
        pairHandle.teardown(
          new SandboxConnectionError(
            "sandboxd port-forward sent an unexpected control frame",
            "protocol",
          ),
        );
        return;
      }
      const channel = raw[0];
      const payload = raw.subarray(1);
      if (channel === PORT_FORWARD_DATA_CHANNEL) {
        const rest = dataHeader.consume(payload);
        if (rest.length > 0 && !socket.destroyed) {
          const ok = socket.write(rest);
          if (!ok) real.pause();
        }
      } else if (channel === PORT_FORWARD_ERROR_CHANNEL) {
        const rest = errorHeader.consume(payload);
        if (rest.length === 0) {
          // Header-only frame on the error channel is normal, not a failure.
          return;
        }
        if (
          errorChunks.reduce((n, c) => n + c.length, 0) <
          MAX_ERROR_CHANNEL_PAYLOAD_BYTES
        ) {
          errorChunks.push(rest);
        }
        const combined = truncateUtf8(
          Buffer.concat(errorChunks)
            .subarray(0, MAX_ERROR_CHANNEL_PAYLOAD_BYTES)
            .toString("utf-8"),
          ERROR_DETAIL_MAX_BYTES,
        );
        pairHandle.teardown(
          new SandboxConnectionError(
            "sandboxd port-forward reported an error",
            "port_forward",
            { detail: combined },
          ),
        );
      } else {
        pairHandle.teardown(
          new SandboxConnectionError(
            `sandboxd port-forward sent an unknown channel (${channel})`,
            "protocol",
          ),
        );
      }
    });

    return pairHandle;
  }

  /** Wires the TCP->WS direction once the handshake has succeeded. */
  private wireSocketToWs(
    socket: net.Socket,
    real: WebSocket,
    pairHandle: PairHandle,
  ): void {
    socket.on("close", () => pairHandle.teardownGraceful());
    socket.on("error", () => pairHandle.teardown());
    socket.on("data", (chunk: Buffer) => {
      // A graceful teardown started by the other side (e.g. the WS already
      // closed and `socket` is mid-flush via socket.end()) means there's
      // nowhere left to send this: real.send() would error and escalate to
      // a forceful teardown, destroying the socket mid-flush and reopening
      // the exact data-loss bug this guard exists to prevent.
      if (pairHandle.settling) return;
      const framed = Buffer.concat([
        Buffer.from([PORT_FORWARD_DATA_CHANNEL]),
        chunk,
      ]);
      if (
        real.bufferedAmount + framed.length >
        TUNNEL_BACKPRESSURE_THRESHOLD_BYTES
      ) {
        socket.pause();
      }
      try {
        real.send(framed, (err?: Error) => {
          if (err) {
            pairHandle.teardown(err);
            return;
          }
          if (real.bufferedAmount <= TUNNEL_BACKPRESSURE_THRESHOLD_BYTES) {
            socket.resume();
          }
        });
      } catch (err) {
        pairHandle.teardown(err);
      }
    });
  }
}
