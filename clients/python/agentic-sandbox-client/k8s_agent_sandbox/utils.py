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

"""Utility functions for the Kubernetes Agent Sandbox Python client."""

from collections.abc import Mapping, Sequence
from datetime import datetime, timedelta, timezone
import ipaddress


def construct_sandbox_claim_lifecycle_spec(shutdown_after_seconds: int) -> dict[str, str]:
    """Construct a SandboxClaim lifecycle spec dict from a TTL in seconds.

    Returns a dict suitable for inclusion as ``spec.lifecycle`` in a
    SandboxClaim manifest, with ``shutdownTime`` set to *now + TTL* (UTC)
    and ``shutdownPolicy`` set to ``"Delete"``.

    Raises ``ValueError`` if the input is not a positive integer or is
    too large for datetime arithmetic.
    """
    if type(shutdown_after_seconds) is not int:
        raise ValueError(
            f"shutdown_after_seconds must be an integer, got {type(shutdown_after_seconds).__name__}"
        )
    if shutdown_after_seconds <= 0:
        raise ValueError(
            f"shutdown_after_seconds must be positive, got {shutdown_after_seconds}"
        )
    try:
        shutdown_time = datetime.now(timezone.utc) + timedelta(seconds=shutdown_after_seconds)
    except OverflowError:
        raise ValueError(
            f"shutdown_after_seconds is too large: {shutdown_after_seconds}"
        ) from None
    return {
        "shutdownTime": shutdown_time.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "shutdownPolicy": "Delete",
    }


def select_pod_ip(ips: Sequence[object] | None) -> str | None:
    """Selects a prioritized and normalized Pod IP address from a list of IPs.

    Scans the list of IP entries, validates them, and returns the
    normalized/canonical IP address string (preferring IPv4 over IPv6).

    The elements in the input list can be:
    - String representation of IP addresses (e.g. "10.0.0.1").
    - Mappings containing an "ip" key (e.g. {"ip": "10.0.0.1"}).
    - Objects containing an "ip" attribute.

    In dual-stack environments, we explicitly prefer IPv4 over IPv6.
    If no IPv4 is found, it falls back to the first syntactically valid IP.
    IPv4-mapped IPv6 addresses (e.g., "::ffff:10.0.0.1") are normalized and
    returned as standard IPv4 addresses (e.g., "10.0.0.1").
    """
    if not ips:
        return None

    first_valid: str | None = None
    for ip_entry in ips:
        ip_str = None
        if isinstance(ip_entry, str):
            ip_str = ip_entry
        elif isinstance(ip_entry, Mapping):
            ip_str = ip_entry.get("ip")
        elif ip_entry is not None:
            ip_str = getattr(ip_entry, "ip", None)

        if not isinstance(ip_str, str) or not ip_str:
            continue
        cleaned = ip_str.strip()
        if not cleaned:
            continue
        try:
            parsed = ipaddress.ip_address(cleaned)
            if parsed.version == 4:
                return str(parsed)
            if parsed.version == 6 and parsed.ipv4_mapped:
                return str(parsed.ipv4_mapped)

            if first_valid is None:
                first_valid = str(parsed)
        except ValueError:
            continue

    return first_valid
