# Copyright 2025 The Kubernetes Authors.
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


import math
import os
import ipaddress
import re
import secrets

import httpx
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import StreamingResponse

# Initialize the FastAPI application
app = FastAPI()

# Configuration
MIN_TCP_PORT = 1
MAX_TCP_PORT = 65535

DEFAULT_SANDBOX_PORT = 8888
DEFAULT_NAMESPACE = "default"
DEFAULT_PROXY_TIMEOUT = 180.0
DEFAULT_CLUSTER_DOMAIN = "cluster.local"
DEFAULT_MAX_KEEPALIVE_CONNECTIONS = 20


def _get_proxy_timeout() -> float:
    raw = os.environ.get("PROXY_TIMEOUT_SECONDS")
    if raw is None:
        return DEFAULT_PROXY_TIMEOUT
    try:
        value = float(raw)
    except (ValueError, TypeError):
        print(f"WARNING: Invalid PROXY_TIMEOUT_SECONDS='{raw}', "
              f"falling back to {DEFAULT_PROXY_TIMEOUT}s")
        return DEFAULT_PROXY_TIMEOUT
    if value <= 0:
        print(f"WARNING: PROXY_TIMEOUT_SECONDS must be positive, got {value}, "
              f"falling back to {DEFAULT_PROXY_TIMEOUT}s")
        return DEFAULT_PROXY_TIMEOUT
    return value


def _get_cluster_domain() -> str:
    cluster_domain = os.environ.get("CLUSTER_DOMAIN")
    if cluster_domain is None:
        return DEFAULT_CLUSTER_DOMAIN
    if cluster_domain == "":
        print("WARNING: CLUSTER_DOMAIN must not be an empty string, "
              f"falling back to {DEFAULT_CLUSTER_DOMAIN}")
        return DEFAULT_CLUSTER_DOMAIN
    return cluster_domain


def _get_max_keepalive_connections() -> int:
    raw = os.environ.get("MAX_KEEPALIVE_CONNECTIONS")
    if raw is None:
        return DEFAULT_MAX_KEEPALIVE_CONNECTIONS
    try:
        value = int(raw)
    except (ValueError, TypeError):
        print(f"WARNING: Invalid MAX_KEEPALIVE_CONNECTIONS='{raw}', "
              f"falling back to {DEFAULT_MAX_KEEPALIVE_CONNECTIONS}")
        return DEFAULT_MAX_KEEPALIVE_CONNECTIONS
    if value < 0:
        print(f"WARNING: MAX_KEEPALIVE_CONNECTIONS must be >= 0, got {value}, "
              f"falling back to {DEFAULT_MAX_KEEPALIVE_CONNECTIONS}")
        return DEFAULT_MAX_KEEPALIVE_CONNECTIONS
    return value


def _get_request_timeout(request: Request) -> float:
    raw = request.headers.get("X-Sandbox-Timeout")
    if raw is None:
        return proxy_timeout
    try:
        value = float(raw)
    except (ValueError, TypeError):
        print(
            f"WARNING: Invalid X-Sandbox-Timeout='{raw}', "
            f"falling back to {proxy_timeout}s"
        )
        return proxy_timeout
    if not math.isfinite(value) or value <= 0:
        print(
            f"WARNING: X-Sandbox-Timeout must be finite and positive, got {value}, "
            f"falling back to {proxy_timeout}s"
        )
        return proxy_timeout
    if value > proxy_timeout:
        print(
            f"WARNING: X-Sandbox-Timeout={value} exceeds configured "
            f"proxy timeout {proxy_timeout}s; capping to {proxy_timeout}s"
        )
        return proxy_timeout
    return value


cluster_domain = _get_cluster_domain()

DNS_LABEL_REGEX = re.compile(r"^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$")


def _is_valid_dns_label(label: str) -> bool:
    if not label or len(label) > 63:
        return False
    return bool(DNS_LABEL_REGEX.match(label))


def _env_var_is_truthy(name: str) -> bool:
    raw = os.environ.get(name)
    if raw is None:
        return False
    return raw.strip().lower() in {"1", "true", "t", "yes", "y", "on"}
proxy_timeout = _get_proxy_timeout()
max_keepalive_connections = _get_max_keepalive_connections()
client = httpx.AsyncClient(
    timeout=proxy_timeout,
    limits=httpx.Limits(max_keepalive_connections=max_keepalive_connections),
)

ROUTER_AUTH_TOKEN = os.environ.get("ROUTER_AUTH_TOKEN", "").strip() or None
ALLOW_UNAUTHENTICATED_ROUTER = _env_var_is_truthy("ALLOW_UNAUTHENTICATED_ROUTER")

print(f"Sandbox router configured with proxy timeout: {proxy_timeout}s")
print(f"Sandbox router configured with max_keepalive_connections: {max_keepalive_connections}")
print(f"Sandbox router configured with cluster_domain: {cluster_domain}")
if ROUTER_AUTH_TOKEN:
    print("Authentication enabled: requests must include valid Bearer token.")
elif ALLOW_UNAUTHENTICATED_ROUTER:
    print("WARNING: Running in UNAUTHENTICATED mode because "
          "ALLOW_UNAUTHENTICATED_ROUTER is enabled. Anyone can use this proxy!")
else:
    raise RuntimeError(
        "ROUTER_AUTH_TOKEN must be set to start the sandbox router securely. "
        "If you intentionally need unauthenticated mode for local development or testing, "
        "set ALLOW_UNAUTHENTICATED_ROUTER=true explicitly."
    )


@app.get("/healthz")
async def health_check():
    """A simple health check endpoint that always returns 200 OK."""
    return {"status": "ok"}


@app.api_route("/{full_path:path}", methods=['GET', 'POST', 'PUT', 'DELETE', 'PATCH'])
async def proxy_request(request: Request, full_path: str):
    """
    Receives all incoming requests, determines the target sandbox from headers,
    and asynchronously proxies the request to it.
    """
    # Check authentication if enabled
    if ROUTER_AUTH_TOKEN:
        auth_header = request.headers.get("Authorization")
        if not auth_header:
            raise HTTPException(
                status_code=401,
                detail="Missing or invalid Authorization header.",
            )
        parts = auth_header.split()
        if len(parts) != 2 or parts[0].lower() != "bearer":
            raise HTTPException(
                status_code=401,
                detail="Missing or invalid Authorization header.",
            )
        if not secrets.compare_digest(parts[1], ROUTER_AUTH_TOKEN):
            raise HTTPException(status_code=401, detail="Invalid token.")

    sandbox_id = request.headers.get("X-Sandbox-ID")
    if not sandbox_id:
        raise HTTPException(
            status_code=400, detail="X-Sandbox-ID header is required.")

    # Sanitize sandbox_id to prevent DNS injection and directory traversal style attacks
    if not _is_valid_dns_label(sandbox_id):
        raise HTTPException(status_code=400, detail="Invalid sandbox ID format.")

    # Dynamic discovery via headers
    namespace = request.headers.get("X-Sandbox-Namespace", DEFAULT_NAMESPACE)
    
    # Sanitize namespace to prevent DNS injection
    if not _is_valid_dns_label(namespace):
        raise HTTPException(status_code=400, detail="Invalid namespace format.")

    try:
        port = int(request.headers.get("X-Sandbox-Port", DEFAULT_SANDBOX_PORT))
        if not (MIN_TCP_PORT <= port <= MAX_TCP_PORT):
            raise ValueError()
    except ValueError:
        raise HTTPException(status_code=400, detail="Invalid port format.")

    # Dynamic routing: route by Pod IP if provided by client, otherwise fallback to DNS name
    pod_ip = request.headers.get("X-Sandbox-Pod-IP")
    if pod_ip:
        try:
            ip = ipaddress.ip_address(pod_ip)
            if ip.is_loopback or ip.is_link_local or ip.is_multicast or ip.is_unspecified:
                raise HTTPException(status_code=400, detail="Invalid target IP address.")
            target_host = f"[{ip}]" if isinstance(ip, ipaddress.IPv6Address) else str(ip)
        except ValueError:
            raise HTTPException(status_code=400, detail="Invalid target IP address format.")
    else:
        # Construct the K8s internal DNS name
        target_host = f"{sandbox_id}.{namespace}.svc.{cluster_domain}"

    target_url = str(
        request.url.replace(scheme="http", hostname=target_host, port=port)
    )

    print(f"Proxying request for sandbox '{sandbox_id}' to URL: {target_url}")

    try:
        timeout = _get_request_timeout(request)
        headers = {
            key: value
            for (key, value) in request.headers.items()
            if key.lower() not in {"host", "authorization"}
        }

        # Request-level timeouts are attached via HTTPX request extensions.
        # The effective value is capped by the router's configured proxy timeout.
        # https://www.python-httpx.org/advanced/extensions/
        req = client.build_request(
            method=request.method,
            url=target_url,
            headers=headers,
            content=request.stream(),
            timeout=httpx.Timeout(timeout, connect=min(timeout, 5.0)),
        )

        resp = await client.send(req, stream=True)

        async def stream_generator():
            try:
                async for chunk in resp.aiter_bytes():
                    yield chunk
            finally:
                await resp.aclose()

        return StreamingResponse(
            content=stream_generator(),
            status_code=resp.status_code,
            headers=dict(resp.headers)
        )
    except httpx.ConnectError as e:
        print(
            f"ERROR: Connection to sandbox at {target_url} failed. Error: {e}")
        raise HTTPException(
            status_code=502,
            detail=f"Could not connect to the backend sandbox: {sandbox_id}",
        )
    except httpx.TimeoutException as e:
        print(
            f"ERROR: Request to sandbox at {target_url} timed out. Error: {e}")
        raise HTTPException(
            status_code=504,
            detail=f"Timed out waiting for the backend sandbox: {sandbox_id}",
        ) from e
    except Exception as e:
        print(f"An unexpected error occurred: {e}")
        raise HTTPException(
            status_code=500,
            detail="An internal error occurred in the proxy.",
        ) from e
