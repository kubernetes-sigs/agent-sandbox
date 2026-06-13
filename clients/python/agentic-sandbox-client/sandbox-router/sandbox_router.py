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


import asyncio
import ipaddress
import os
import re
import secrets
from dataclasses import dataclass
from urllib.parse import urlunparse

import httpx
import websockets
from fastapi import FastAPI, Request, HTTPException, WebSocket, status
from fastapi.responses import StreamingResponse
from websockets.exceptions import WebSocketException

# Initialize the FastAPI application
app = FastAPI()

# Configuration
MIN_TCP_PORT = 1
MAX_TCP_PORT = 65535

DEFAULT_SANDBOX_PORT = 8888
DEFAULT_NAMESPACE = "default"
DEFAULT_PROXY_TIMEOUT = 180.0
DEFAULT_CLUSTER_DOMAIN = "cluster.local"
DEFAULT_ROUTER_AUTH_HEADER = "Authorization"

HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}
ROUTING_HEADERS = {
    "x-sandbox-id",
    "x-sandbox-namespace",
    "x-sandbox-port",
    "x-sandbox-pod-ip",
}


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


def _get_router_auth_header() -> str:
    raw = os.environ.get("ROUTER_AUTH_HEADER")
    if raw is None:
        return DEFAULT_ROUTER_AUTH_HEADER
    value = raw.strip()
    if value == "":
        print("WARNING: ROUTER_AUTH_HEADER must not be an empty string, "
              f"falling back to {DEFAULT_ROUTER_AUTH_HEADER}")
        return DEFAULT_ROUTER_AUTH_HEADER
    if not re.match(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$", value):
        print(f"WARNING: Invalid ROUTER_AUTH_HEADER='{raw}', "
              f"falling back to {DEFAULT_ROUTER_AUTH_HEADER}")
        return DEFAULT_ROUTER_AUTH_HEADER
    return value


cluster_domain = _get_cluster_domain()
router_auth_header = _get_router_auth_header()

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
client = httpx.AsyncClient(timeout=proxy_timeout)

ROUTER_AUTH_TOKEN = os.environ.get("ROUTER_AUTH_TOKEN", "").strip() or None
ALLOW_UNAUTHENTICATED_ROUTER = _env_var_is_truthy("ALLOW_UNAUTHENTICATED_ROUTER")

print(f"Sandbox router configured with proxy timeout: {proxy_timeout}s")
print(f"Sandbox router configured with cluster_domain: {cluster_domain}")
print(f"Sandbox router configured with auth header: {router_auth_header}")
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


@dataclass(frozen=True)
class SandboxRoute:
    sandbox_id: str
    target_host: str
    port: int


def _authenticate(headers) -> None:
    if not ROUTER_AUTH_TOKEN:
        return
    auth_header = headers.get(router_auth_header)
    if not auth_header:
        raise HTTPException(
            status_code=401,
            detail=f"Missing or invalid {router_auth_header} header.",
        )
    parts = auth_header.split()
    if len(parts) != 2 or parts[0].lower() != "bearer":
        raise HTTPException(
            status_code=401,
            detail=f"Missing or invalid {router_auth_header} header.",
        )
    if not secrets.compare_digest(parts[1], ROUTER_AUTH_TOKEN):
        raise HTTPException(status_code=401, detail="Invalid token.")


def _sandbox_route_from_headers(headers) -> SandboxRoute:
    sandbox_id = headers.get("X-Sandbox-ID")
    if not sandbox_id:
        raise HTTPException(
            status_code=400, detail="X-Sandbox-ID header is required.")

    # Sanitize sandbox_id to prevent DNS injection and directory traversal style attacks
    if not _is_valid_dns_label(sandbox_id):
        raise HTTPException(status_code=400, detail="Invalid sandbox ID format.")

    # Dynamic discovery via headers
    namespace = headers.get("X-Sandbox-Namespace", DEFAULT_NAMESPACE)

    # Sanitize namespace to prevent DNS injection
    if not _is_valid_dns_label(namespace):
        raise HTTPException(status_code=400, detail="Invalid namespace format.")

    try:
        port = int(headers.get("X-Sandbox-Port", DEFAULT_SANDBOX_PORT))
        if not (MIN_TCP_PORT <= port <= MAX_TCP_PORT):
            raise ValueError()
    except ValueError:
        raise HTTPException(status_code=400, detail="Invalid port format.")

    # Dynamic routing: route by Pod IP if provided by client, otherwise fallback to DNS name
    pod_ip = headers.get("X-Sandbox-Pod-IP")
    if pod_ip:
        try:
            ip = ipaddress.ip_address(pod_ip)
            if ip.is_loopback or ip.is_link_local or ip.is_multicast or ip.is_unspecified:
                raise HTTPException(status_code=400, detail="Invalid target IP address.")
            target_host = str(ip)
        except ValueError:
            raise HTTPException(status_code=400, detail="Invalid target IP address format.")
    else:
        # Construct the K8s internal DNS name
        target_host = f"{sandbox_id}.{namespace}.svc.{cluster_domain}"

    return SandboxRoute(sandbox_id=sandbox_id, target_host=target_host, port=port)


def _forward_headers(headers) -> dict[str, str]:
    stripped = {
        "host",
        router_auth_header.lower(),
        *HOP_BY_HOP_HEADERS,
        *ROUTING_HEADERS,
    }
    return {
        key: value
        for (key, value) in headers.items()
        if key.lower() not in stripped
    }


def _http_target_url(request: Request, route: SandboxRoute) -> str:
    return str(
        request.url.replace(scheme="http", hostname=route.target_host, port=route.port)
    )


def _ws_target_url(websocket: WebSocket, route: SandboxRoute, full_path: str) -> str:
    path = "/" + full_path.lstrip("/")
    return urlunparse(
        (
            "ws",
            f"{route.target_host}:{route.port}",
            path,
            "",
            websocket.url.query,
            "",
        )
    )


@app.api_route("/{full_path:path}", methods=['GET', 'POST', 'PUT', 'DELETE', 'PATCH'])
async def proxy_request(request: Request, full_path: str):
    """
    Receives all incoming requests, determines the target sandbox from headers,
    and asynchronously proxies the request to it.
    """
    _authenticate(request.headers)
    route = _sandbox_route_from_headers(request.headers)
    target_url = _http_target_url(request, route)

    print(f"Proxying request for sandbox '{route.sandbox_id}' to URL: {target_url}")

    try:
        headers = _forward_headers(request.headers)

        req = client.build_request(
            method=request.method,
            url=target_url,
            headers=headers,
            content=request.stream()
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
            detail=f"Could not connect to the backend sandbox: {route.sandbox_id}",
        )
    except Exception as e:
        print(f"An unexpected error occurred: {e}")
        raise HTTPException(
            status_code=500, detail="An internal error occurred in the proxy.")


@app.websocket("/{full_path:path}")
async def proxy_websocket(websocket: WebSocket, full_path: str):
    """
    Receives a WebSocket connection, determines the target sandbox from headers,
    and proxies frames bidirectionally to it.
    """
    try:
        _authenticate(websocket.headers)
        route = _sandbox_route_from_headers(websocket.headers)
    except HTTPException as e:
        await websocket.close(
            code=status.WS_1008_POLICY_VIOLATION,
            reason=str(e.detail),
        )
        return

    target_url = _ws_target_url(websocket, route, full_path)
    print(f"Proxying websocket for sandbox '{route.sandbox_id}' to URL: {target_url}")

    try:
        upstream = await websockets.connect(
            target_url,
            additional_headers=_forward_headers(websocket.headers),
            open_timeout=proxy_timeout,
            ping_interval=None,
        )
    except (OSError, TimeoutError, WebSocketException) as e:
        print(
            f"ERROR: WebSocket connection to sandbox at {target_url} failed. Error: {e}")
        await websocket.close(code=status.WS_1011_INTERNAL_ERROR)
        return

    await websocket.accept()
    try:
        try:
            await _relay_websockets(client_ws=websocket, upstream_ws=upstream)
        finally:
            await upstream.close()
            await _safe_websocket_close(websocket, code=status.WS_1000_NORMAL_CLOSURE)
    except Exception as e:
        print(f"An unexpected websocket proxy error occurred: {e}")
        await _safe_websocket_close(websocket, code=status.WS_1011_INTERNAL_ERROR)


async def _relay_websockets(*, client_ws: WebSocket, upstream_ws) -> None:
    client_to_upstream = asyncio.create_task(
        _client_to_upstream(client_ws=client_ws, upstream_ws=upstream_ws)
    )
    upstream_to_client = asyncio.create_task(
        _upstream_to_client(client_ws=client_ws, upstream_ws=upstream_ws)
    )
    done, pending = await asyncio.wait(
        {client_to_upstream, upstream_to_client},
        return_when=asyncio.FIRST_COMPLETED,
    )
    for task in pending:
        task.cancel()
    await asyncio.gather(*pending, return_exceptions=True)
    for task in done:
        task.result()


async def _client_to_upstream(*, client_ws: WebSocket, upstream_ws) -> None:
    while True:
        message = await client_ws.receive()
        if message["type"] == "websocket.disconnect":
            await upstream_ws.close()
            return
        text = message.get("text")
        if text is not None:
            await upstream_ws.send(text)
            continue
        data = message.get("bytes")
        if data is not None:
            await upstream_ws.send(data)


async def _upstream_to_client(*, client_ws: WebSocket, upstream_ws) -> None:
    async for message in upstream_ws:
        if isinstance(message, str):
            await client_ws.send_text(message)
        else:
            await client_ws.send_bytes(message)


async def _safe_websocket_close(websocket: WebSocket, *, code: int) -> None:
    try:
        await websocket.close(code=code)
    except RuntimeError:
        return
