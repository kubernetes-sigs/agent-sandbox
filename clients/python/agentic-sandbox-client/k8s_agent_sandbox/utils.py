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

from collections.abc import AsyncIterator, Iterator, Mapping, Sequence
from contextlib import asynccontextmanager, contextmanager, suppress
from datetime import datetime, timedelta, timezone
import functools
import inspect
import ipaddress
import json
import os
import tempfile
import time
from typing import Any

from .constants import SANDBOX_NAME_HASH_LABEL
from .models import SandboxClaimEnvVar


def merge_headers(*layers: Mapping[str, str] | None) -> dict[str, str]:
    """Merge header mappings case-insensitively; later layers win.

    ``None`` layers are skipped. Returns a new dict.
    """
    merged: dict[str, tuple[str, str]] = {}
    for layer in layers:
        for name, value in (layer or {}).items():
            merged[name.lower()] = (name, value)
    return dict(merged.values())


def record_latency(metric):
    """Decorator to measure and record execution latency to a Prometheus metric.

    Does not record metrics if self._skip_latency_metric is set to True during execution.
    """
    def decorator(func):
        @functools.wraps(func)
        def wrapper(self, *args, **kwargs):
            self._skip_latency_metric = False
            start_time = time.perf_counter()
            response = None
            try:
                response = func(self, *args, **kwargs)
                return response
            except Exception as e:
                if not getattr(self, "_skip_latency_metric", False):
                    status = "failure"
                    duration = (time.perf_counter() - start_time) * 1000.0
                    metric.labels(status=status).observe(duration)
                raise
            finally:
                if response is not None and not getattr(self, "_skip_latency_metric", False):
                    duration = (time.perf_counter() - start_time) * 1000.0
                    status = "success" if getattr(response, "success", False) else "failure"
                    metric.labels(status=status).observe(duration)
        return wrapper
    return decorator

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
            if isinstance(parsed, ipaddress.IPv6Address) and parsed.ipv4_mapped:
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


def extract_sandbox_name_hash(sandbox_object: dict[str, Any]) -> str | None:
    status = sandbox_object.get("status") or {}
    selector = status.get("selector") or ""
    for requirement in selector.split(","):
        key, sep, value = requirement.partition("=")
        if sep and key.strip() == SANDBOX_NAME_HASH_LABEL:
            return value.strip() or None

    return None


def construct_sandbox_claim_env_spec(env: Mapping[str, str] | None) -> list[SandboxClaimEnvVar]:
    """Construct SandboxClaim env spec entries from a mapping of names to values."""
    if not env:
        return []

    return [
        SandboxClaimEnvVar(name=name, value=value)
        for name, value in env.items()
    ]


def kubeconfig_from_configuration(
    configuration: Any,
    authorization: str | None,
    default_headers: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    """Build a kubeconfig for the cluster a Kubernetes ``Configuration`` targets.

    ``authorization`` is the resolved ``Authorization`` header value. Only a
    bearer token is carried over; basic auth is not. ``default_headers`` are
    the ``ApiClient``'s; only the impersonation user and group are carried over.
    """
    cluster: dict[str, Any] = {"server": configuration.host}
    # kubectl rejects a CA with insecure-skip-tls-verify. The Python clients
    # ignore the CA when verify_ssl is off, so do the same.
    if not configuration.verify_ssl:
        cluster["insecure-skip-tls-verify"] = True
    elif configuration.ssl_ca_cert:
        cluster["certificate-authority"] = configuration.ssl_ca_cert
    if getattr(configuration, "tls_server_name", None):
        cluster["tls-server-name"] = configuration.tls_server_name
    if getattr(configuration, "proxy", None):
        cluster["proxy-url"] = configuration.proxy

    user: dict[str, Any] = {}
    if configuration.cert_file:
        user["client-certificate"] = configuration.cert_file
    if configuration.key_file:
        user["client-key"] = configuration.key_file
    scheme, _, credential = (authorization or "").partition(" ")
    if scheme.lower() == "bearer" and credential:
        user["token"] = credential
    headers = {k.lower(): v for k, v in (default_headers or {}).items()}
    if headers.get("impersonate-user"):
        user["as"] = headers["impersonate-user"]
    if headers.get("impersonate-group"):
        user["as-groups"] = [headers["impersonate-group"]]

    return {
        "apiVersion": "v1",
        "kind": "Config",
        "clusters": [{"name": "sandbox", "cluster": cluster}],
        "users": [{"name": "sandbox", "user": user}],
        "contexts": [
            {"name": "sandbox", "context": {"cluster": "sandbox", "user": "sandbox"}}
        ],
        "current-context": "sandbox",
    }


# load_kube_config stores a token under BearerToken. Older kubernetes releases
# and hand-built configurations use authorization.
_AUTHORIZATION_KEYS = ("BearerToken", "authorization")


@contextmanager
def _temporary_kubeconfig(
    api_client: Any, authorization: str | None
) -> Iterator[list[str]]:
    # mkstemp creates the file 0600, and it can hold a bearer token.
    fd, path = tempfile.mkstemp(prefix="sandbox-kubeconfig-", suffix=".json")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(
                kubeconfig_from_configuration(
                    api_client.configuration, authorization, api_client.default_headers
                ),
                f,
            )
        yield ["--kubeconfig", path]
    finally:
        with suppress(FileNotFoundError):
            os.unlink(path)


@contextmanager
def kubectl_kubeconfig_args(api_client: Any | None) -> Iterator[list[str]]:
    """Yield the ``kubectl`` flags that target an injected ``ApiClient``'s cluster.

    ``kubectl`` otherwise uses the ambient kubeconfig, which may be a different
    cluster than the one ``api_client`` talks to. Yields no flags when
    ``api_client`` is None. The kubeconfig is deleted on exit, which is safe
    once ``kubectl`` has started because it reads the file only at startup.
    """
    if api_client is None:
        yield []
        return
    configuration = api_client.configuration
    authorization = None
    for key in _AUTHORIZATION_KEYS:
        authorization = configuration.get_api_key_with_prefix(key)
        if authorization:
            break
    with _temporary_kubeconfig(api_client, authorization) as args:
        yield args


@asynccontextmanager
async def async_kubectl_kubeconfig_args(api_client: Any | None) -> AsyncIterator[list[str]]:
    """Async variant of :func:`kubectl_kubeconfig_args` for ``kubernetes_asyncio``."""
    if api_client is None:
        yield []
        return
    configuration = api_client.configuration
    authorization = None
    for key in _AUTHORIZATION_KEYS:
        # kubernetes_asyncio runs a possibly async refresh hook here.
        authorization = configuration.get_api_key_with_prefix(key)
        if inspect.isawaitable(authorization):
            authorization = await authorization
        if authorization:
            break
    with _temporary_kubeconfig(api_client, authorization) as args:
        yield args
