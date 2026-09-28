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

export const CLAIM_API_GROUP = "extensions.agents.x-k8s.io";
export const CLAIM_API_VERSION = "v1beta1";
export const CLAIM_PLURAL_NAME = "sandboxclaims";

export const SANDBOX_API_GROUP = "agents.x-k8s.io";
export const SANDBOX_API_VERSION = "v1beta1";
export const SANDBOX_PLURAL_NAME = "sandboxes";

export const POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name";

// Maximum time (ms) allowed for cleanup operations (claim deletion, in-flight drain)
export const CLEANUP_TIMEOUT_MS = 5_000;

// -----------------------------------------------------------------------------
// sandboxd connectivity layer defaults (SandboxdOptions in types.ts)
// -----------------------------------------------------------------------------

export const DEFAULT_SANDBOXD_REST_PORT = 8080;
export const DEFAULT_SANDBOXD_GRPC_PORT = 9090;

// Budget for the shared connect (both port-forward listeners + REST health
// check) and for each individual WS handshake within it. Re-applied in full
// to every reconnect attempt.
export const DEFAULT_PORT_FORWARD_READY_TIMEOUT_MS = 30_000;

export const DEFAULT_MAX_DOWNLOAD_SIZE = 256 * 1024 * 1024;
export const DEFAULT_MAX_UPLOAD_SIZE = 256 * 1024 * 1024;
export const DEFAULT_MAX_METADATA_RESPONSE_SIZE = 8 * 1024 * 1024;
// Applies to the fully decoded ExecuteResponse (stdout + stderr + protobuf
// overhead), not to stdout alone.
export const DEFAULT_MAX_COMMAND_OUTPUT_SIZE = 4 * 1024 * 1024;

// Per-call default for files/run operations, covering the whole call —
// dependency import, connect wait, and response processing.
export const DEFAULT_OPERATION_TIMEOUT_MS = 60_000;

// Truncation limit applied to any server-controlled diagnostic text (REST
// error bodies, gRPC rawMessage, port-forward error-channel payloads) before
// it is attached to a public error's `detail`.
export const ERROR_DETAIL_MAX_BYTES = 512;

// -----------------------------------------------------------------------------
// PodTunnel framing (see tunnel.ts)
// -----------------------------------------------------------------------------

// kubectl port-forward sub-protocol channel numbers: one pair of "channels"
// per forwarded port, data first then error, in the order ports were
// requested. PodTunnel always requests exactly one port per WebSocket, so
// each connection only ever uses channel 0 (data) and channel 1 (error).
export const PORT_FORWARD_DATA_CHANNEL = 0;
export const PORT_FORWARD_ERROR_CHANNEL = 1;

// Non-empty error-channel payloads are diagnostic text, not stream data; cap
// how much of it is retained before the pair is torn down.
export const MAX_ERROR_CHANNEL_PAYLOAD_BYTES = 4 * 1024;

// Backpressure threshold (bytes) for the TCP<->WebSocket pump in each
// direction. Bounds queued-but-unsent bytes, not total transfer size.
export const TUNNEL_BACKPRESSURE_THRESHOLD_BYTES = 1024 * 1024;

// SandboxClaim Ready=False reasons the claim controller will not recover from on
// its own (see computeReadyCondition in
// extensions/controllers/sandboxclaim_controller.go). Watch-based ready-waits
// fail fast on these instead of burning the full timeout. Transient reasons
// (AdoptionPending, SandboxMissing, SandboxNotReady, ReconcilerError) are
// intentionally absent: the controller retries those. Kept in sync with the
// Python SDK's TERMINAL_CLAIM_READY_REASONS.
export const TERMINAL_CLAIM_READY_REASONS: ReadonlySet<string> = new Set([
  "InvalidMetadata",
  "EnvVarsInjectionRejected",
  "VolumeClaimTemplatesError",
  "ClaimExpired", // extensions ClaimExpiredReason
  "SandboxExpired", // core SandboxReasonExpired, forwarded to the claim
]);
