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
import os
import re
import secrets
import ipaddress

import httpx
import websockets
from fastapi import FastAPI, Request, HTTPException, WebSocket, WebSocketDisconnect
from fastapi.responses import StreamingResponse
from websockets.exceptions import ConnectionClosed

# Initialize the FastAPI application
app = FastAPI()

# Configuration
MIN_TCP_PORT = 1
MAX_TCP_PORT = 65535

DEFAULT_SANDBOX_PORT = 8888
DEFAULT_NAMESPACE = "default"
DEFAULT_PROXY_TIMEOUT = 180.0
DEFAULT_CLUSTER_DOMAIN = "cluster.local"

ROUTER_HEADER_NAMES = frozenset({
    "x-sandbox-id",
    "x-sandbox-namespace",
    "x-sandbox-port",
    "x-sandbox-pod-ip",
})

HOP_BY_HOP_HEADERS = frozenset({
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
})

WEBSOCKET_HANDSHAKE_HEADERS = frozenset({
    "sec-websocket-key",
    "sec-websocket-version",
    "sec-websocket-extensions",
    "sec-websocket-protocol",
})


class RoutingError(Exception):
    """Invalid sandbox routing headers."""


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


def _resolve_target(headers, url, scheme: str) -> tuple[str, str]:
    """Return the backend URL and sandbox ID from routing headers."""
    sandbox_id = headers.get("X-Sandbox-ID")
    if not sandbox_id:
        raise RoutingError("X-Sandbox-ID header is required.")
    if not _is_valid_dns_label(sandbox_id):
        raise RoutingError("Invalid sandbox ID format.")

    namespace = headers.get("X-Sandbox-Namespace", DEFAULT_NAMESPACE)
    if not _is_valid_dns_label(namespace):
        raise RoutingError("Invalid namespace format.")

    try:
        port = int(headers.get("X-Sandbox-Port", DEFAULT_SANDBOX_PORT))
        if not (MIN_TCP_PORT <= port <= MAX_TCP_PORT):
            raise ValueError()
    except ValueError:
        raise RoutingError("Invalid port format.")

    pod_ip = headers.get("X-Sandbox-Pod-IP")
    if pod_ip:
        try:
            ip = ipaddress.ip_address(pod_ip)
            if ip.is_loopback or ip.is_link_local or ip.is_multicast or ip.is_unspecified:
                raise RoutingError("Invalid target IP address.")
            target_host = f"[{ip}]" if ip.version == 6 else str(ip)
        except ValueError:
            raise RoutingError("Invalid target IP address format.")
    else:
        target_host = f"{sandbox_id}.{namespace}.svc.{cluster_domain}"

    target_url = str(url.replace(scheme=scheme, hostname=target_host, port=port))
    return target_url, sandbox_id


def _proxy_headers(headers, *, websocket: bool = False) -> dict[str, str]:
    excluded = ROUTER_HEADER_NAMES | HOP_BY_HOP_HEADERS | {"host", "authorization"}
    if websocket:
        excluded |= WEBSOCKET_HANDSHAKE_HEADERS

    return {
        key: value
        for key, value in headers.items()
        if key.lower() not in excluded
    }


def _response_headers(headers) -> dict[str, str]:
    return {
        key: value
        for key, value in headers.items()
        if key.lower() not in HOP_BY_HOP_HEADERS
    }


def _url_for_log(target_url: str) -> str:
    """Return target_url without the query string; queries may carry secrets."""
    return target_url.split("?", 1)[0]


def _log_proxy_target(sandbox_id: str, target_url: str, *, protocol: str) -> None:
    print(
        f"Proxying {protocol} for sandbox '{sandbox_id}' "
        f"to URL: {_url_for_log(target_url)}"
    )


def _check_router_auth(headers) -> None:
    """Raise HTTPException when authentication is enabled and the token is invalid.

    Auth uses a bearer token in the Authorization header rather than cookies, so
    cross-site WebSocket hijacking (CSWSH) is not a practical risk today: browsers
    do not auto-attach bearer tokens on cross-origin connections. If auth ever moves
    to cookies, add an Origin allowlist on the WebSocket route.
    """
    if not ROUTER_AUTH_TOKEN:
        return

    auth_header = headers.get("Authorization")
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


proxy_timeout = _get_proxy_timeout()
client = httpx.AsyncClient(timeout=proxy_timeout)

ROUTER_AUTH_TOKEN = os.environ.get("ROUTER_AUTH_TOKEN", "").strip() or None
ALLOW_UNAUTHENTICATED_ROUTER = _env_var_is_truthy("ALLOW_UNAUTHENTICATED_ROUTER")

print(f"Sandbox router configured with proxy timeout: {proxy_timeout}s")
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


async def _relay_websocket(client_ws: WebSocket, backend_ws) -> None:
    """Bidirectionally relay messages between client and backend WebSockets."""

    async def client_to_backend() -> None:
        try:
            while True:
                message = await client_ws.receive()
                if message["type"] == "websocket.disconnect":
                    break
                if "text" in message:
                    await backend_ws.send(message["text"])
                elif "bytes" in message:
                    await backend_ws.send(message["bytes"])
        except WebSocketDisconnect:
            pass

    async def backend_to_client() -> None:
        try:
            async for message in backend_ws:
                if isinstance(message, str):
                    await client_ws.send_text(message)
                else:
                    await client_ws.send_bytes(message)
        except ConnectionClosed:
            pass

    client_task = asyncio.create_task(client_to_backend())
    backend_task = asyncio.create_task(backend_to_client())
    done, pending = await asyncio.wait(
        [client_task, backend_task],
        return_when=asyncio.FIRST_COMPLETED,
    )
    for task in pending:
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
    for task in done:
        try:
            await task
        except (WebSocketDisconnect, ConnectionClosed):
            pass


@app.websocket("/{full_path:path}")
async def proxy_websocket(websocket: WebSocket, full_path: str):
    """
    Proxies WebSocket connections to the target sandbox.

    HTTP reverse proxies cannot forward 101 Switching Protocols responses, so
    WebSocket traffic must use this dedicated endpoint.
    """
    try:
        _check_router_auth(websocket.headers)
    except HTTPException as exc:
        await websocket.close(code=1008, reason=exc.detail)
        return

    try:
        target_url, sandbox_id = _resolve_target(
            websocket.headers,
            websocket.url,
            "ws",
        )
    except RoutingError as exc:
        await websocket.close(code=1008, reason=str(exc))
        return

    _log_proxy_target(sandbox_id, target_url, protocol="WebSocket")

    subprotocol_header = websocket.headers.get("sec-websocket-protocol")
    subprotocols = None
    if subprotocol_header:
        subprotocols = [item.strip() for item in subprotocol_header.split(",") if item.strip()]

    try:
        async with websockets.connect(
            target_url,
            additional_headers=_proxy_headers(websocket.headers, websocket=True),
            subprotocols=subprotocols,
            open_timeout=proxy_timeout,
        ) as backend_ws:
            selected_subprotocol = backend_ws.subprotocol
            await websocket.accept(subprotocol=selected_subprotocol)
            await _relay_websocket(websocket, backend_ws)
    except websockets.InvalidStatus as exc:
        print(
            f"ERROR: WebSocket handshake to sandbox at {_url_for_log(target_url)} failed. "
            f"Error: {exc}"
        )
        await websocket.close(code=1011, reason="Backend WebSocket handshake failed.")
    except OSError as exc:
        print(
            f"ERROR: Connection to sandbox at {_url_for_log(target_url)} failed. "
            f"Error: {exc}"
        )
        await websocket.close(
            code=1011,
            reason=f"Could not connect to the backend sandbox: {sandbox_id}",
        )
    except Exception as exc:
        print(f"An unexpected WebSocket error occurred: {exc}")
        await websocket.close(code=1011, reason="An internal error occurred in the proxy.")


@app.api_route("/{full_path:path}", methods=['GET', 'POST', 'PUT', 'DELETE', 'PATCH'])
async def proxy_request(request: Request, full_path: str):
    """
    Receives all incoming requests, determines the target sandbox from headers,
    and asynchronously proxies the request to it.
    """
    _check_router_auth(request.headers)

    try:
        target_url, sandbox_id = _resolve_target(
            request.headers,
            request.url,
            "http",
        )
    except RoutingError as exc:
        raise HTTPException(status_code=400, detail=str(exc))

    _log_proxy_target(sandbox_id, target_url, protocol="request")

    try:
        req = client.build_request(
            method=request.method,
            url=target_url,
            headers=_proxy_headers(request.headers),
            content=request.stream()
        )

        resp = await client.send(req, stream=True)

        if resp.status_code == 101:
            await resp.aclose()
            raise HTTPException(
                status_code=502,
                detail=(
                    "Backend attempted a WebSocket upgrade over HTTP. "
                    "Connect using the WebSocket protocol instead."
                ),
            )

        async def stream_generator():
            try:
                async for chunk in resp.aiter_bytes():
                    yield chunk
            finally:
                await resp.aclose()

        return StreamingResponse(
            content=stream_generator(),
            status_code=resp.status_code,
            headers=_response_headers(resp.headers)
        )
    except httpx.ConnectError as e:
        print(
            f"ERROR: Connection to sandbox at {_url_for_log(target_url)} failed. "
            f"Error: {e}"
        )
        raise HTTPException(
            status_code=502,
            detail=f"Could not connect to the backend sandbox: {sandbox_id}",
        )
    except HTTPException:
        raise
    except Exception as e:
        print(f"An unexpected error occurred: {e}")
        raise HTTPException(
            status_code=500, detail="An internal error occurred in the proxy.")
