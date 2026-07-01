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
import ssl
from typing import TYPE_CHECKING, Union

if TYPE_CHECKING:
    from .models import TLSConfig


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


def is_valid_ip(s: str) -> bool:
    if not isinstance(s, str):
        return False
    import ipaddress
    try:
        ipaddress.ip_address(s)
        return True
    except ValueError:
        return False


def _is_integer_label(label: str) -> bool:
    if not label:
        return False
    if label.isdigit():
        return True
    if label.lower().startswith("0x"):
        val = label[2:]
        return len(val) == 0 or all(c in "0123456789abcdef" for c in val.lower())
    return False


def is_valid_gateway_hostname(s: str) -> bool:
    if not isinstance(s, str):
        return False
    if not s or len(s) > 253:
        return False
    labels = s.split('.')
    # Reject hostnames that consist entirely of integer labels (decimal or hex)
    # as they can resolve to loopback or other IP addresses via libc resolvers.
    if all(_is_integer_label(label) for label in labels):
        return False
    # Enforce DNS label length limit of 63 characters (RFC 1123 / RFC 1035)
    if any(len(label) > 63 for label in labels):
        return False
    for i, c in enumerate(s):
        if 'a' <= c <= 'z' or 'A' <= c <= 'Z' or '0' <= c <= '9':
            continue
        elif c == '-':
            if i == 0 or s[i-1] == '.':
                return False
        elif c == '.':
            if i == 0 or s[i-1] == '.' or s[i-1] == '-':
                return False
        else:
            return False
    last = s[-1]
    return last != '-' and last != '.'


def build_base_url(scheme: str, host: str, port: int | None = None) -> str:
    """Build a base URL, wrapping IPv6 hosts in brackets.

    If port is None, no ":port" suffix is appended (use for Gateway URLs that
    rely on the scheme's default port: 80 for http, 443 for https).
    """
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    if port is None:
        return f"{scheme}://{host}"
    return f"{scheme}://{host}:{port}"


def _looks_like_pem(s: str) -> bool:
    return "-----BEGIN" in s


def build_ssl_context(tls: "TLSConfig | None") -> Union[bool, ssl.SSLContext]:
    """Build a value suitable for httpx ``verify=`` or an HTTPS adapter/transport.

    Returns:
        - ``True`` when no TLS config is provided (use system default CAs).
        - ``False`` when ``insecure_skip_verify`` is set (disabled verification).
        - An ``ssl.SSLContext`` intended for passing into an HTTPS adapter/transport
          when a custom CA is provided. The CA may be either a file path or
          inline PEM content; the form is auto-detected.

    Raises ``ValueError`` on malformed CA content.
    """
    if tls is None:
        return True
    if tls.insecure_skip_verify:
        return False

    if tls.ca_cert:
        ctx = ssl.create_default_context()
        if _looks_like_pem(tls.ca_cert):
            try:
                ctx.load_verify_locations(cadata=tls.ca_cert)
            except ssl.SSLError as e:
                raise ValueError(f"TLSConfig.ca_cert PEM is invalid: {e}") from e
        else:
            try:
                ctx.load_verify_locations(cafile=tls.ca_cert)
            except (FileNotFoundError, ssl.SSLError, OSError) as e:
                raise ValueError(
                    f"TLSConfig.ca_cert path is unreadable or invalid: {e}"
                ) from e
        return ctx

    return True


def requests_verify_value(tls: "TLSConfig | None") -> Union[bool, str]:
    """Return a value suitable for ``requests.Session.verify``.

    requests does not accept an ``ssl.SSLContext`` directly. For inline PEM
    content we materialize it to a temp file managed by the caller; for file
    paths we pass them through. For consistency, callers should use
    ``apply_tls_to_requests_session`` instead of this helper directly so the
    temp-file lifecycle is handled.
    """
    if tls is None:
        return True
    if tls.insecure_skip_verify:
        return False
    if tls.ca_cert and not _looks_like_pem(tls.ca_cert):
        return tls.ca_cert
    # PEM path is handled separately by apply_tls_to_requests_session.
    return True


def apply_tls_to_requests_session(session, tls: "TLSConfig | None") -> str | None:
    """Configure a ``requests.Session`` to honor a ``TLSConfig``.

    Returns the path of a temporary PEM file if one was created, so the caller
    can delete it on close. Returns None otherwise.

    Note: requests does not natively support SNI override on a Session; for
    server_name_override the caller must mount a custom adapter (handled in
    the connector). This helper only deals with verify / CA bundle.
    """
    if tls is None:
        return None
    if tls.insecure_skip_verify:
        session.verify = False
        return None
    if not tls.ca_cert:
        return None
    if not _looks_like_pem(tls.ca_cert):
        session.verify = tls.ca_cert
        return None
    # PEM string: materialize to a temp file so requests can load it.
    import tempfile
    fd, path = tempfile.mkstemp(suffix=".pem", prefix="agent-sandbox-ca-")
    try:
        with open(fd, "w") as f:
            f.write(tls.ca_cert)
    except Exception:
        import os
        try:
            os.unlink(path)
        except OSError:
            pass
        raise
    session.verify = path
    return path
