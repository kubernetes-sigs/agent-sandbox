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

export interface SandboxClientOptions {
  namespace?: string;
  sandboxReadyTimeout?: number;
  enableTracing?: boolean;
  traceServiceName?: string;
  /**
   * Logger used for diagnostic output (lifecycle events, retries, warnings).
   * Defaults to a logger that writes to stderr. Mirrors the Go client's
   * Options.Logger.
   */
  logger?: Logger;
  /**
   * Suppresses the default stderr logger. Ignored when `logger` is set.
   * Mirrors the Go client's Options.Quiet.
   */
  quiet?: boolean;
  /**
   * Configures the connection to the sandboxd runtime (files/commands) that
   * every Sandbox handle created or attached by this client uses.
   */
  sandboxd?: SandboxdOptions;
}

/**
 * Configures the lazily-established connection each Sandbox handle uses to
 * reach sandboxd (REST files API + gRPC process API) inside its Pod.
 *
 * Every field defaults only when `undefined`; explicit 0, NaN,
 * Infinity, or a negative value is rejected at construction time. Ports must
 * be integers in [1, 65535]; size limits must be positive safe integers;
 * `*TimeoutMs` fields must be integers in [1, 2147483647].
 */
export interface SandboxdOptions {
  /**
   * How the SDK reaches sandboxd. Default: "port-forward". See
   * {@link SandboxdConnectivity}.
   */
  connectivity?: SandboxdConnectivity;
  /** sandboxd's REST files/health port inside the Pod. Default: 8080. */
  restPort?: number;
  /** sandboxd's gRPC process port inside the Pod. Default: 9090. */
  grpcPort?: number;
  /**
   * Budget (ms) for establishing the shared connection, through a
   * successful /v1/health check: for "port-forward" it starts when both
   * local listeners are opened; for the in-cluster modes it bounds the
   * health polling against the pod address. Re-applied in full on every
   * reconnect. Default: 30000.
   */
  portForwardReadyTimeoutMs?: number;
  /**
   * Maximum bytes accepted from files.read() / files.readStream().
   * Default: 256 MiB.
   */
  maxDownloadSize?: number;
  /**
   * Maximum bytes sent by files.write() / files.writeStream().
   * Default: 256 MiB.
   */
  maxUploadSize?: number;
  /**
   * Maximum bytes accepted for JSON response bodies (list, health, error
   * detail previews). Default: 8 MiB.
   */
  maxMetadataResponseSize?: number;
  /**
   * Maximum decoded size of a single ExecuteResponse — stdout + stderr +
   * protobuf overhead combined, not stdout alone. Must fit the gRPC
   * transport's receive limit (at most 0xffffffff). Default: 4 MiB.
   */
  maxCommandOutputSize?: number;
}

/**
 * Transport used to reach sandboxd. Same values as the Go client's
 * Connectivity.
 *
 * - `"port-forward"`: a WebSocket port-forward brokered by the apiserver.
 *   Works from anywhere a kubeconfig does, including a laptop or CI runner.
 * - `"in-cluster-service"`: dials the Sandbox's headless Service by its
 *   in-cluster DNS name (status.serviceFQDN), taking the apiserver off the
 *   data path. The Service only ever selects its own Sandbox's pod, and a
 *   deleted Sandbox takes its Service with it, so connections fail rather
 *   than land on another pod that inherited the IP (DNS caching still leaves
 *   a TTL-bounded window). Requires `spec.service: true` on the template;
 *   createSandbox()/getSandbox() throw SandboxNoServiceError when the Sandbox
 *   has no Service instead of falling back to the pod IP.
 * - `"in-cluster-pod-ip"`: dials status.podIPs (IPv4 preferred). Needs no
 *   Service, but nothing detects that the pod was rescheduled: requests can
 *   continue to a stale address Kubernetes may have reassigned to an
 *   unrelated pod.
 *
 * Both in-cluster modes require this process to run inside the same cluster
 * as the sandbox pods, and send REST and gRPC in plaintext across the pod
 * network.
 */
export type SandboxdConnectivity =
  | "port-forward"
  | "in-cluster-service"
  | "in-cluster-pod-ip";

/** Options accepted by every sandbox.files.* method. */
export interface FileCallOptions {
  /**
   * Total budget (ms) for this call, from entry through response
   * processing — including any time spent waiting on the shared connect.
   * Default: 60000.
   */
  timeoutMs?: number;
  signal?: AbortSignal;
}

/** Options accepted by sandbox.commands.run(). */
export interface RunOptions {
  /** See {@link FileCallOptions.timeoutMs}. Default: 60000. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

export interface WriteOptions extends FileCallOptions {
  /** POSIX file mode, e.g. "0644". Must match `^0[0-7]{3}$`. */
  mode?: string;
}

export interface DeleteOptions extends FileCallOptions {
  /** Delete non-empty directories recursively. Default: false. */
  recursive?: boolean;
}

/** Result of sandbox.commands.run(). A non-zero exitCode is not an error. */
export interface ExecutionResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

/** One row of a DirectoryListing returned by sandbox.files.list(). */
export interface FileEntry {
  name: string;
  size: number;
  type: "file" | "directory";
  /** Validated RFC3339 timestamp string, not converted to a Date. */
  modifiedAt: string;
  /** Octal POSIX mode, e.g. "0644", when the server reports one. */
  mode?: string;
}

/** Result of sandbox.files.list(). */
export interface DirectoryListing {
  path: string;
  entries: FileEntry[];
}

/**
 * Diagnostic logger injectable via SandboxClientOptions.logger.
 * Mirrors the Go client's logr.Logger usage (Options.Logger).
 */
export interface Logger {
  debug(message: string): void;
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
}

export interface CreateSandboxOptions {
  sandboxReadyTimeout?: number;
  labels?: Record<string, string>;
  /**
   * Delete the claim and its sandbox after this many seconds, measured from
   * the createSandbox call (including provisioning time). Sets the claim's
   * `spec.lifecycle.shutdownTime` to that absolute deadline and its
   * `spec.lifecycle.shutdownPolicy` to `"Delete"`. Must be a positive integer
   * that produces a valid RFC3339 deadline. Omit to leave expiration unset.
   */
  shutdownAfterSeconds?: number;
}
