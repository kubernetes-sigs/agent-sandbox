#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Turn nono's machine-readable audit verification into demo assertions."""

import json
import sys
from pathlib import Path


def require(value, message):
    """Exit with an audit failure when an expected condition is false."""
    if not value:
        print(f"[audit-fail] {message}", file=sys.stderr)
        raise SystemExit(1)


if len(sys.argv) != 4:
    raise SystemExit("usage: verify-audit.py VERIFY_JSON SESSION_JSON SESSION_ID")

verify = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
session = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
expected_session_id = sys.argv[3]

integrity = verify.get("session", {})
ledger = verify.get("ledger", {})
attestation = verify.get("attestation", {})

require(session.get("session_id") == expected_session_id, "verified the wrong session")
require(session.get("ended"), "session was not finalized")
require(integrity.get("records_verified"), "event records did not verify")
require(integrity.get("event_count_matches"), "stored event count did not match")
require(ledger.get("session_found"), "session was missing from the audit ledger")
require(ledger.get("session_digest_matches"), "ledger session digest did not match")
require(ledger.get("ledger_chain_verified"), "global audit ledger chain did not verify")
require(attestation.get("present"), "signed audit attestation was missing")
require(attestation.get("signature_verified"), "DSSE signature did not verify")
require(attestation.get("key_id_matches"), "attested signing-key identity did not match")
require(attestation.get("merkle_root_matches"), "attested Merkle root did not match")
require(attestation.get("session_id_matches"), "attested session id did not match")
require(
    attestation.get("expected_public_key_matches"),
    "signature did not match the separately pinned public key",
)

network_events = len(session.get("network_events", []))
command_events = len(session.get("command_policy_events", []))
require(network_events >= 2, "expected network policy decisions were not captured")
require(command_events >= 3, "expected command-policy decisions were not captured")

print(
    f"[audit-ok] captured {integrity.get('event_count', 0)} events "
    f"({network_events} network, {command_events} command-policy)"
)
print("[audit-ok] event chain, Merkle root, and audit ledger verified")
print("[audit-ok] DSSE signature matched the pinned public key")
