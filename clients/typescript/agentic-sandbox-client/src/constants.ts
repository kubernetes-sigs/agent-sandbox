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
